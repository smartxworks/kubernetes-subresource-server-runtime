/*
Copyright 2021.

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

package subresourceserver

type ServerOption interface {
	apply(*serverConfig)
}

func WithAddr(addr string) ServerOption {
	return newFuncServerOption(func(o *serverConfig) {
		o.addr = addr
	})
}

func WithCertDir(certDir string) ServerOption {
	return newFuncServerOption(func(o *serverConfig) {
		o.certDir = certDir
	})
}

func WithCertFileName(certFileName string) ServerOption {
	return newFuncServerOption(func(o *serverConfig) {
		o.certFileName = certFileName
	})
}

func WithKeyFileName(keyFileName string) ServerOption {
	return newFuncServerOption(func(o *serverConfig) {
		o.keyFileName = keyFileName
	})
}

type serverConfig struct {
	addr         string
	certDir      string
	certFileName string
	keyFileName  string
}

var defaultServerConfig = serverConfig{
	addr:         ":8443",
	certDir:      "/tmp/k8s-subresource-server/cert",
	certFileName: "tls.crt",
	keyFileName:  "tls.key",
}

func newFuncServerOption(f func(*serverConfig)) *funcServerOption {
	return &funcServerOption{
		f: f,
	}
}

type funcServerOption struct {
	f func(*serverConfig)
}

func (f *funcServerOption) apply(opts *serverConfig) {
	f.f(opts)
}
