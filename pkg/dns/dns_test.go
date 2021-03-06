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
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	skymsg "github.com/skynetservices/skydns/msg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
)

const (
	testDomain    = "cluster.local."
	testService   = "testservice"
	testNamespace = "default"
)

func newKubeDNS() *KubeDNS {
	kd := &KubeDNS{
		domain:         testDomain,
		endpointsStore: cache.NewStore(cache.MetaNamespaceKeyFunc),
		servicesStore:  cache.NewStore(cache.MetaNamespaceKeyFunc),
		cache:          NewTreeCache(),
		cacheLock:      sync.RWMutex{},
		domainPath:     reverseArray(strings.Split(strings.TrimRight(testDomain, "."), ".")),
	}
	return kd
}

func TestPodDns(t *testing.T) {
	const (
		testPodIP      = "1.2.3.4"
		sanitizedPodIP = "1-2-3-4"
	)
	kd := newKubeDNS()

	records, err := kd.Records(sanitizedPodIP+".default.pod."+kd.domain, false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records))
	assert.Equal(t, testPodIP, records[0].Host)
}

func TestUnnamedSinglePortService(t *testing.T) {
	kd := newKubeDNS()
	s := newService(testNamespace, testService, "1.2.3.4", "", 80)
	// Add the service
	kd.newService(s)
	assertDNSForClusterIP(t, kd, s)
	// Delete the service
	kd.removeService(s)
	assertNoDNSForClusterIP(t, kd, s)
}

func TestNamedSinglePortService(t *testing.T) {
	const (
		portName1 = "http1"
		portName2 = "http2"
	)
	kd := newKubeDNS()
	s := newService(testNamespace, testService, "1.2.3.4", portName1, 80)
	// Add the service
	kd.newService(s)
	assertDNSForClusterIP(t, kd, s)
	assertSRVForNamedPort(t, kd, s, portName1)

	newService := *s
	// update the portName of the service
	newService.Spec.Ports[0].Name = portName2
	kd.updateService(s, &newService)
	assertDNSForClusterIP(t, kd, s)
	assertSRVForNamedPort(t, kd, s, portName2)
	assertNoSRVForNamedPort(t, kd, s, portName1)

	// Delete the service
	kd.removeService(s)
	assertNoDNSForClusterIP(t, kd, s)
	assertNoSRVForNamedPort(t, kd, s, portName1)
	assertNoSRVForNamedPort(t, kd, s, portName2)
}

func TestHeadlessService(t *testing.T) {
	kd := newKubeDNS()
	s := newHeadlessService()
	assert.NoError(t, kd.servicesStore.Add(s))
	endpoints := newEndpoints(s, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"), newSubsetWithOnePort("", 8080, "10.0.0.3", "10.0.0.4"))

	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.newService(s)
	assertDNSForHeadlessService(t, kd, endpoints)
	kd.removeService(s)
	assertNoDNSForHeadlessService(t, kd, s)
}

func TestHeadlessServiceWithNamedPorts(t *testing.T) {
	kd := newKubeDNS()
	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))
	endpoints := newEndpoints(service, newSubsetWithTwoPorts("http1", 80, "http2", 81, "10.0.0.1", "10.0.0.2"),
		newSubsetWithOnePort("https", 443, "10.0.0.3", "10.0.0.4"))

	// We expect 10 records. 6 SRV records. 4 POD records.
	// add endpoints
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertSRVForHeadlessService(t, kd, service, endpoints)

	// reduce endpoints
	endpoints.Subsets = endpoints.Subsets[:1]
	kd.handleEndpointAdd(endpoints)
	// We expect 6 records. 4 SRV records. 2 POD records.
	assertDNSForHeadlessService(t, kd, endpoints)
	assertSRVForHeadlessService(t, kd, service, endpoints)

	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
}

func TestHeadlessServiceEndpointsUpdate(t *testing.T) {
	kd := newKubeDNS()
	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	endpoints := newEndpoints(service, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"))
	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)

	// increase endpoints
	endpoints.Subsets = append(endpoints.Subsets,
		newSubsetWithOnePort("", 8080, "10.0.0.3", "10.0.0.4"),
	)
	// expected DNSRecords = 4
	kd.handleEndpointAdd(endpoints)
	assertDNSForHeadlessService(t, kd, endpoints)

	// remove all endpoints
	endpoints.Subsets = []kapi.EndpointSubset{}
	kd.handleEndpointAdd(endpoints)
	assertNoDNSForHeadlessService(t, kd, service)

	// remove service
	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
}

func TestHeadlessServiceWithDelayedEndpointsAddition(t *testing.T) {
	kd := newKubeDNS()
	// create service
	service := newHeadlessService()

	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	// add service
	kd.newService(service)
	assertNoDNSForHeadlessService(t, kd, service)

	// create endpoints
	endpoints := newEndpoints(service, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"))

	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add endpoints
	kd.handleEndpointAdd(endpoints)

	assertDNSForHeadlessService(t, kd, endpoints)

	// remove service
	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
}

func newService(namespace, serviceName, clusterIP, portName string, portNumber int32) *kapi.Service {
	service := kapi.Service{
		ObjectMeta: kapi.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
		Spec: kapi.ServiceSpec{
			ClusterIP: clusterIP,
			Ports: []kapi.ServicePort{
				{Port: portNumber, Name: portName, Protocol: "TCP"},
			},
		},
	}
	return &service
}

func newHeadlessService() *kapi.Service {
	service := kapi.Service{
		ObjectMeta: kapi.ObjectMeta{
			Name:      testService,
			Namespace: testNamespace,
		},
		Spec: kapi.ServiceSpec{
			ClusterIP: "None",
			Ports: []kapi.ServicePort{
				{Port: 0},
			},
		},
	}
	return &service
}

func newEndpoints(service *kapi.Service, subsets ...kapi.EndpointSubset) *kapi.Endpoints {
	endpoints := kapi.Endpoints{
		ObjectMeta: service.ObjectMeta,
		Subsets:    []kapi.EndpointSubset{},
	}

	endpoints.Subsets = append(endpoints.Subsets, subsets...)
	return &endpoints
}

func newSubsetWithOnePort(portName string, port int32, ips ...string) kapi.EndpointSubset {
	subset := newSubset()
	subset.Ports = append(subset.Ports, kapi.EndpointPort{Port: port, Name: portName, Protocol: "TCP"})
	for _, ip := range ips {
		subset.Addresses = append(subset.Addresses, kapi.EndpointAddress{IP: ip})
	}
	return subset
}

func newSubsetWithTwoPorts(portName1 string, portNumber1 int32, portName2 string, portNumber2 int32, ips ...string) kapi.EndpointSubset {
	subset := newSubsetWithOnePort(portName1, portNumber1, ips...)
	subset.Ports = append(subset.Ports, kapi.EndpointPort{Port: portNumber2, Name: portName2, Protocol: "TCP"})
	return subset
}

func newSubset() kapi.EndpointSubset {
	subset := kapi.EndpointSubset{
		Addresses: []kapi.EndpointAddress{},
		Ports:     []kapi.EndpointPort{},
	}
	return subset
}

func assertSRVForHeadlessService(t *testing.T, kd *KubeDNS, s *kapi.Service, e *kapi.Endpoints) {
	for _, subset := range e.Subsets {
		for _, port := range subset.Ports {
			records, err := kd.Records(getSRVFQDN(kd, s, port.Name), false)
			require.NoError(t, err)
			assertRecordPortsMatchPort(t, port.Port, records)
			assertCNameRecordsMatchEndpointIPs(t, kd, subset.Addresses, records)
		}
	}
}

func assertDNSForHeadlessService(t *testing.T, kd *KubeDNS, e *kapi.Endpoints) {
	records, err := kd.Records(getEndpointsFQDN(kd, e), false)
	require.NoError(t, err)
	endpoints := map[string]bool{}
	for _, subset := range e.Subsets {
		for _, endpointAddress := range subset.Addresses {
			endpoints[endpointAddress.IP] = true
		}
	}
	assert.Equal(t, len(endpoints), len(records))
	for _, record := range records {
		_, found := endpoints[record.Host]
		assert.True(t, found)
	}
}

func assertRecordPortsMatchPort(t *testing.T, port int32, records []skymsg.Service) {
	for _, record := range records {
		assert.Equal(t, port, int32(record.Port))
	}
}

func assertCNameRecordsMatchEndpointIPs(t *testing.T, kd *KubeDNS, e []kapi.EndpointAddress, records []skymsg.Service) {
	endpoints := map[string]bool{}
	for _, endpointAddress := range e {
		endpoints[endpointAddress.IP] = true
	}
	assert.Equal(t, len(e), len(records), "unexpected record count")
	for _, record := range records {
		_, found := endpoints[getIPForCName(t, kd, record.Host)]
		assert.True(t, found, "Did not find endpoint with address:%s", record.Host)
	}
}

func getIPForCName(t *testing.T, kd *KubeDNS, cname string) string {
	records, err := kd.Records(cname, false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records), "Could not get IP for CNAME record for %s", cname)
	assert.NotNil(t, net.ParseIP(records[0].Host), "Invalid IP address %q", records[0].Host)
	return records[0].Host
}

func assertNoDNSForHeadlessService(t *testing.T, kd *KubeDNS, s *kapi.Service) {
	records, err := kd.Records(getServiceFQDN(kd, s), false)
	require.Error(t, err)
	assert.Equal(t, 0, len(records))
}

func assertSRVForNamedPort(t *testing.T, kd *KubeDNS, s *kapi.Service, portName string) {
	records, err := kd.Records(getSRVFQDN(kd, s, portName), false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records))
	assert.Equal(t, getServiceFQDN(kd, s), records[0].Host)
}

func assertNoSRVForNamedPort(t *testing.T, kd *KubeDNS, s *kapi.Service, portName string) {
	records, err := kd.Records(getSRVFQDN(kd, s, portName), false)
	require.Error(t, err)
	assert.Equal(t, 0, len(records))
}

func assertNoDNSForClusterIP(t *testing.T, kd *KubeDNS, s *kapi.Service) {
	serviceFQDN := getServiceFQDN(kd, s)
	queries := getEquivalentQueries(serviceFQDN, s.Namespace)
	for _, query := range queries {
		records, err := kd.Records(query, false)
		require.Error(t, err)
		assert.Equal(t, 0, len(records))
	}
}

func assertDNSForClusterIP(t *testing.T, kd *KubeDNS, s *kapi.Service) {
	serviceFQDN := getServiceFQDN(kd, s)
	queries := getEquivalentQueries(serviceFQDN, s.Namespace)
	for _, query := range queries {
		records, err := kd.Records(query, false)
		require.NoError(t, err)
		assert.Equal(t, 1, len(records))
		assert.Equal(t, s.Spec.ClusterIP, records[0].Host)
	}
}

func getEquivalentQueries(serviceFQDN, namespace string) []string {
	return []string{
		serviceFQDN,
		strings.Replace(serviceFQDN, ".svc.", ".*.", 1),
		strings.Replace(serviceFQDN, namespace, "*", 1),
		strings.Replace(strings.Replace(serviceFQDN, namespace, "*", 1), ".svc.", ".*.", 1),
		"*." + serviceFQDN,
	}
}

func getServiceFQDN(kd *KubeDNS, s *kapi.Service) string {
	return fmt.Sprintf("%s.%s.svc.%s", s.Name, s.Namespace, kd.domain)
}

func getEndpointsFQDN(kd *KubeDNS, e *kapi.Endpoints) string {
	return fmt.Sprintf("%s.%s.svc.%s", e.Name, e.Namespace, kd.domain)
}

func getSRVFQDN(kd *KubeDNS, s *kapi.Service, portName string) string {
	return fmt.Sprintf("_%s._tcp.%s.%s.svc.%s", portName, s.Name, s.Namespace, kd.domain)
}
