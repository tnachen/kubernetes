/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dns

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"sync"
	"time"

	etcd "github.com/coreos/etcd/client"
	"github.com/golang/glog"
	skymsg "github.com/skynetservices/skydns/msg"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/endpoints"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kframework "k8s.io/kubernetes/pkg/controller/framework"
	kselector "k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/util/validation"
	"k8s.io/kubernetes/pkg/util/wait"
)

const (
	kubernetesSvcName = "kubernetes"

	// A subdomain added to the user specified domain for all services.
	serviceSubdomain = "svc"

	// A subdomain added to the user specified dmoain for all pods.
	podSubdomain = "pod"

	// Resync period for the kube controller loop.
	resyncPeriod = 5 * time.Minute
)

type KubeDNS struct {
	// kubeClient makes calls to API Server and registers calls with API Server
	// to get Endpoints and Service objects.
	kubeClient *kclient.Client

	// The domain for which this DNS Server is authoritative.
	domain string

	// A cache that contains all the endpoints in the system.
	endpointsStore kcache.Store

	// A cache that contains all the services in the system.
	servicesStore kcache.Store

	// stores DNS records for the domain.
	// A Records and SRV Records for (regular) services and headless Services.
	cache *TreeCache

	// caller is responsible for using the cacheLock before invoking methods on cache
	// the cache is not thread-safe, and the caller can guarantee thread safety by using
	// the cacheLock
	cacheLock sync.RWMutex

	// The domain for which this DNS Server is authoritative, in array format and reversed.
	// e.g. if domain is "cluster.local", domainPath is []string{"local", "cluster"}
	domainPath []string

	// endpointsController  invokes registered callbacks when endpoints change.
	endpointsController *kframework.Controller

	// serviceController invokes registered callbacks when services change.
	serviceController *kframework.Controller
}

func NewKubeDNS(client *kclient.Client, domain string) *KubeDNS {
	kd := &KubeDNS{
		kubeClient: client,
		domain:     domain,
		cache:      NewTreeCache(),
		cacheLock:  sync.RWMutex{},
		domainPath: reverseArray(strings.Split(strings.TrimRight(domain, "."), ".")),
	}
	kd.setEndpointsStore()
	kd.setServicesStore()
	return kd
}

func (kd *KubeDNS) Start() {
	go kd.endpointsController.Run(wait.NeverStop)
	go kd.serviceController.Run(wait.NeverStop)
	// Wait synchronously for the Kubernetes service and add a DNS record for it.
	// This ensures that the Start function returns only after having received Service objects
	// from APIServer.
	// TODO: we might not have to wait for kubernetes service specifically. We should just wait
	// for a list operation to be complete from APIServer.
	kd.waitForKubernetesService()
}

func (kd *KubeDNS) waitForKubernetesService() (svc *kapi.Service) {
	name := fmt.Sprintf("%v/%v", kapi.NamespaceDefault, kubernetesSvcName)
	glog.Infof("Waiting for service: %v", name)
	var err error
	servicePollInterval := 1 * time.Second
	for {
		svc, err = kd.kubeClient.Services(kapi.NamespaceDefault).Get(kubernetesSvcName)
		if err != nil || svc == nil {
			glog.Infof("Ignoring error while waiting for service %v: %v. Sleeping %v before retrying.", name, err, servicePollInterval)
			time.Sleep(servicePollInterval)
			continue
		}
		break
	}
	return
}

func (kd *KubeDNS) GetCacheAsJSON() (string, error) {
	kd.cacheLock.RLock()
	defer kd.cacheLock.RUnlock()
	json, err := kd.cache.Serialize()
	return json, err
}

func (kd *KubeDNS) setServicesStore() {
	// Returns a cache.ListWatch that gets all changes to services.
	serviceWatch := kcache.NewListWatchFromClient(kd.kubeClient, "services", kapi.NamespaceAll, kselector.Everything())
	kd.servicesStore, kd.serviceController = kframework.NewInformer(
		serviceWatch,
		&kapi.Service{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc:    kd.newService,
			DeleteFunc: kd.removeService,
			UpdateFunc: kd.updateService,
		},
	)
}

func (kd *KubeDNS) setEndpointsStore() {
	// Returns a cache.ListWatch that gets all changes to endpoints.
	endpointsWatch := kcache.NewListWatchFromClient(kd.kubeClient, "endpoints", kapi.NamespaceAll, kselector.Everything())
	kd.endpointsStore, kd.endpointsController = kframework.NewInformer(
		endpointsWatch,
		&kapi.Endpoints{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc: kd.handleEndpointAdd,
			UpdateFunc: func(oldObj, newObj interface{}) {
				// TODO: Avoid unwanted updates.
				kd.handleEndpointAdd(newObj)
			},
		},
	)
}

func assertIsService(obj interface{}) (*kapi.Service, bool) {
	if service, ok := obj.(*kapi.Service); ok {
		return service, ok
	} else {
		glog.Errorf("Type assertion failed! Expected 'Service', got %T", service)
		return nil, ok
	}
}

func (kd *KubeDNS) newService(obj interface{}) {
	if service, ok := assertIsService(obj); ok {
		// if ClusterIP is not set, a DNS entry should not be created
		if !kapi.IsServiceIPSet(service) {
			kd.newHeadlessService(service)
			return
		}
		if len(service.Spec.Ports) == 0 {
			glog.Warning("Unexpected service with no ports, this should not have happend: %v", service)
		}
		kd.newPortalService(service)
	}
}

func (kd *KubeDNS) removeService(obj interface{}) {
	if s, ok := assertIsService(obj); ok {
		subCachePath := append(kd.domainPath, serviceSubdomain, s.Namespace, s.Name)
		kd.cacheLock.Lock()
		defer kd.cacheLock.Unlock()
		kd.cache.deletePath(subCachePath...)
	}
}

func (kd *KubeDNS) updateService(oldObj, newObj interface{}) {
	kd.newService(newObj)
}

func (kd *KubeDNS) handleEndpointAdd(obj interface{}) {
	if e, ok := obj.(*kapi.Endpoints); ok {
		kd.addDNSUsingEndpoints(e)
	}
}

func (kd *KubeDNS) addDNSUsingEndpoints(e *kapi.Endpoints) error {
	svc, err := kd.getServiceFromEndpoints(e)
	if err != nil {
		return err
	}
	if svc == nil || kapi.IsServiceIPSet(svc) {
		// No headless service found corresponding to endpoints object.
		return nil
	}
	return kd.generateRecordsForHeadlessService(e, svc)
}

func (kd *KubeDNS) getServiceFromEndpoints(e *kapi.Endpoints) (*kapi.Service, error) {
	key, err := kcache.MetaNamespaceKeyFunc(e)
	if err != nil {
		return nil, err
	}
	obj, exists, err := kd.servicesStore.GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get service object from services store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("could not find service for endpoint %q in namespace %q", e.Name, e.Namespace)
		return nil, nil
	}
	if svc, ok := assertIsService(obj); ok {
		return svc, nil
	}
	return nil, fmt.Errorf("got a non service object in services store %v", obj)
}

func (kd *KubeDNS) newPortalService(service *kapi.Service) {
	subCache := NewTreeCache()
	recordValue, recordLabel := getSkyMsg(service.Spec.ClusterIP, 0)
	subCache.setEntry(recordLabel, recordValue)

	// Generate SRV Records
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.Name != "" && port.Protocol != "" {
			srvValue := kd.generateSRVRecordValue(service, int(port.Port))
			subCache.setEntry(recordLabel, srvValue, "_"+strings.ToLower(string(port.Protocol)), "_"+port.Name)
		}
	}
	subCachePath := append(kd.domainPath, serviceSubdomain, service.Namespace)
	kd.cacheLock.Lock()
	defer kd.cacheLock.Unlock()
	kd.cache.setSubCache(service.Name, subCache, subCachePath...)
}

func (kd *KubeDNS) generateRecordsForHeadlessService(e *kapi.Endpoints, svc *kapi.Service) error {
	// TODO: remove this after v1.4 is released and the old annotations are EOL
	podHostnames, err := getPodHostnamesFromAnnotation(e.Annotations)
	if err != nil {
		return err
	}
	subCache := NewTreeCache()
	glog.V(4).Infof("Endpoints Annotations: %v", e.Annotations)
	for idx := range e.Subsets {
		for subIdx := range e.Subsets[idx].Addresses {
			address := &e.Subsets[idx].Addresses[subIdx]
			endpointIP := address.IP
			recordValue, endpointName := getSkyMsg(endpointIP, 0)
			if hostLabel, exists := getHostname(address, podHostnames); exists {
				endpointName = hostLabel
			}
			subCache.setEntry(endpointName, recordValue)
			for portIdx := range e.Subsets[idx].Ports {
				endpointPort := &e.Subsets[idx].Ports[portIdx]
				if endpointPort.Name != "" && endpointPort.Protocol != "" {
					srvValue := kd.generateSRVRecordValue(svc, int(endpointPort.Port), endpointName)
					subCache.setEntry(endpointName, srvValue, "_"+strings.ToLower(string(endpointPort.Protocol)), "_"+endpointPort.Name)
				}
			}
		}
	}
	subCachePath := append(kd.domainPath, serviceSubdomain, svc.Namespace)
	kd.cacheLock.Lock()
	defer kd.cacheLock.Unlock()
	kd.cache.setSubCache(svc.Name, subCache, subCachePath...)
	return nil
}

func getHostname(address *kapi.EndpointAddress, podHostnames map[string]endpoints.HostRecord) (string, bool) {
	if len(address.Hostname) > 0 {
		return address.Hostname, true
	}
	if hostRecord, exists := podHostnames[address.IP]; exists && len(validation.IsDNS1123Label(hostRecord.HostName)) == 0 {
		return hostRecord.HostName, true
	}
	return "", false
}

func getPodHostnamesFromAnnotation(annotations map[string]string) (map[string]endpoints.HostRecord, error) {
	hostnames := map[string]endpoints.HostRecord{}

	if annotations != nil {
		if serializedHostnames, exists := annotations[endpoints.PodHostnamesAnnotation]; exists && len(serializedHostnames) > 0 {
			err := json.Unmarshal([]byte(serializedHostnames), &hostnames)
			if err != nil {
				return nil, err
			}
		}
	}
	return hostnames, nil
}

func (kd *KubeDNS) generateSRVRecordValue(svc *kapi.Service, portNumber int, labels ...string) *skymsg.Service {
	host := strings.Join([]string{svc.Name, svc.Namespace, serviceSubdomain, kd.domain}, ".")
	for _, cNameLabel := range labels {
		host = cNameLabel + "." + host
	}
	recordValue, _ := getSkyMsg(host, portNumber)
	return recordValue
}

// Generates skydns records for a headless service.
func (kd *KubeDNS) newHeadlessService(service *kapi.Service) error {
	// Create an A record for every pod in the service.
	// This record must be periodically updated.
	// Format is as follows:
	// For a service x, with pods a and b create DNS records,
	// a.x.ns.domain. and, b.x.ns.domain.
	key, err := kcache.MetaNamespaceKeyFunc(service)
	if err != nil {
		return err
	}
	e, exists, err := kd.endpointsStore.GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to get endpoints object from endpoints store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("Could not find endpoints for service %q in namespace %q. DNS records will be created once endpoints show up.", service.Name, service.Namespace)
		return nil
	}
	if e, ok := e.(*kapi.Endpoints); ok {
		return kd.generateRecordsForHeadlessService(e, service)
	}
	return nil
}

func (kd *KubeDNS) Records(name string, exact bool) ([]skymsg.Service, error) {
	glog.Infof("Received DNS Request:%s, exact:%v", name, exact)
	trimmed := strings.TrimRight(name, ".")
	segments := strings.Split(trimmed, ".")
	path := reverseArray(segments)
	if kd.isPodRecord(path) {
		ip, err := kd.getPodIP(path)
		if err == nil {
			skyMsg, _ := getSkyMsg(ip, 0)
			return []skymsg.Service{*skyMsg}, nil
		}
		return nil, err
	}

	if exact {
		key := path[len(path)-1]
		if key == "" {
			return []skymsg.Service{}, nil
		}
		kd.cacheLock.RLock()
		defer kd.cacheLock.RUnlock()
		if record, ok := kd.cache.getEntry(key, path[:len(path)-1]...); ok {
			return []skymsg.Service{*(record.(*skymsg.Service))}, nil
		}
		return nil, etcd.Error{Code: etcd.ErrorCodeKeyNotFound}
	}

	kd.cacheLock.RLock()
	defer kd.cacheLock.RUnlock()
	records := kd.cache.getValuesForPathWithWildcards(path...)
	retval := []skymsg.Service{}
	for _, val := range records {
		retval = append(retval, *(val.(*skymsg.Service)))
	}
	glog.Infof("records:%v, retval:%v, path:%v", records, retval, path)
	if len(retval) == 0 {
		return nil, etcd.Error{Code: etcd.ErrorCodeKeyNotFound}
	}
	return retval, nil
}

func (kd *KubeDNS) ReverseRecord(name string) (*skymsg.Service, error) {
	glog.Infof("Received ReverseRecord Request:%s", name)

	segments := strings.Split(strings.TrimRight(name, "."), ".")

	for _, k := range segments {
		if k == "*" {
			return nil, fmt.Errorf("reverse can not contain wildcards")
		}
	}

	return nil, fmt.Errorf("must be exactly one service record")
}

// e.g {"local", "cluster", "pod", "default", "10-0-0-1"}
func (kd *KubeDNS) isPodRecord(path []string) bool {
	if len(path) != len(kd.domainPath)+3 {
		return false
	}
	if path[len(kd.domainPath)] != "pod" {
		return false
	}
	for _, segment := range path {
		if segment == "*" {
			return false
		}
	}
	return true
}

func (kd *KubeDNS) getPodIP(path []string) (string, error) {
	ipStr := path[len(path)-1]
	ip := strings.Replace(ipStr, "-", ".", -1)
	if parsed := net.ParseIP(ip); parsed != nil {
		return ip, nil
	}
	return "", fmt.Errorf("Invalid IP Address %v", ip)
}

// Returns record in a format that SkyDNS understands.
// Also return the hash of the record.
func getSkyMsg(ip string, port int) (*skymsg.Service, string) {
	msg := &skymsg.Service{
		Host:     ip,
		Port:     port,
		Priority: 10,
		Weight:   10,
		Ttl:      30,
	}
	s := fmt.Sprintf("%v", msg)
	h := fnv.New32a()
	h.Write([]byte(s))
	hash := fmt.Sprintf("%x", h.Sum32())
	glog.Infof("DNS Record:%s, hash:%s", s, hash)
	return msg, fmt.Sprintf("%x", hash)
}

func reverseArray(arr []string) []string {
	for i := 0; i < len(arr)/2; i++ {
		j := len(arr) - i - 1
		arr[i], arr[j] = arr[j], arr[i]
	}
	return arr
}
