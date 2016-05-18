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

package options

import (
	"github.com/spf13/pflag"
)

type HorizontalControllerOptions struct {
	Heapster string
}

func NewHorizontalControllerOptions() HorizontalControllerOptions {
	return HorizontalControllerOptions{}
}

func (o *HorizontalControllerOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Heapster, "heapster", o.heapster, "The address of the Heapster server (overrides default pod value)")
}
