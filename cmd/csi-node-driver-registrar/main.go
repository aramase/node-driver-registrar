/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	goflag "flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/kubernetes-csi/csi-lib-utils/metrics"
	"github.com/spf13/pflag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"github.com/kubernetes-csi/csi-lib-utils/connection"
	csirpc "github.com/kubernetes-csi/csi-lib-utils/rpc"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

const (
	// Name of node annotation that contains JSON map of driver names to node
	// names
	annotationKey = "csi.volume.kubernetes.io/nodeid"

	// Default timeout of short CSI calls like GetPluginInfo
	csiTimeout = time.Second

	// Verify (and update, if needed) the node ID at this frequency.
	sleepDuration = 2 * time.Minute
)

// Command line flags
var (
	connectionTimeout       = pflag.Duration("connection-timeout", 0, "The --connection-timeout flag is deprecated")
	csiAddress              = pflag.String("csi-address", "/run/csi/socket", "Path of the CSI driver socket that the node-driver-registrar will connect to.")
	pluginRegistrationPath  = pflag.String("plugin-registration-path", "/registration", "Path to Kubernetes plugin registration directory.")
	kubeletRegistrationPath = pflag.String("kubelet-registration-path", "", "Path of the CSI driver socket on the Kubernetes host machine.")
	healthzPort             = pflag.Int("health-port", 0, "(deprecated) TCP port for healthz requests. Set to 0 to disable the healthz server. Only one of `--health-port` and `--http-endpoint` can be set.")
	httpEndpoint            = pflag.String("http-endpoint", "", "The TCP network address where the HTTP server for diagnostics, including the health check indicating whether the registration socket exists, will listen (example: `:8080`). The default is empty string, which means the server is disabled. Only one of `--health-port` and `--http-endpoint` can be set.")
	showVersion             = pflag.Bool("version", false, "Show version.")
	version                 = "unknown"

	// List of supported versions
	supportedVersions = []string{"1.0.0"}
)

// registrationServer is a sample plugin to work with plugin watcher
type registrationServer struct {
	driverName string
	endpoint   string
	version    []string
}

var _ registerapi.RegistrationServer = registrationServer{}

// NewregistrationServer returns an initialized registrationServer instance
func newRegistrationServer(driverName string, endpoint string, versions []string) registerapi.RegistrationServer {
	return &registrationServer{
		driverName: driverName,
		endpoint:   endpoint,
		version:    versions,
	}
}

// GetInfo is the RPC invoked by plugin watcher
func (e registrationServer) GetInfo(ctx context.Context, req *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	klog.Infof("Received GetInfo call: %+v", req)
	return &registerapi.PluginInfo{
		Type:              registerapi.CSIPlugin,
		Name:              e.driverName,
		Endpoint:          e.endpoint,
		SupportedVersions: e.version,
	}, nil
}

func (e registrationServer) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	klog.Infof("Received NotifyRegistrationStatus call: %+v", status)
	if !status.PluginRegistered {
		klog.Errorf("Registration process failed with error: %+v, restarting registration container.", status.Error)
		os.Exit(1)
	}

	return &registerapi.RegistrationStatusResponse{}, nil
}

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	logOptions := logs.NewOptions()
	logOptions.AddFlags(pflag.CommandLine)

	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	pflag.Set("logtostderr", "true")
	pflag.Parse()

	// set log formatter type
	logOptions.Apply()

	if *kubeletRegistrationPath == "" {
		klog.Error("kubelet-registration-path is a required parameter")
		os.Exit(1)
	}

	if *showVersion {
		fmt.Println(os.Args[0], version)
		return
	}
	klog.Infof("Version: %s", version)

	if *healthzPort > 0 && *httpEndpoint != "" {
		klog.Error("only one of `--health-port` and `--http-endpoint` can be set.")
		os.Exit(1)
	}
	var addr string
	if *healthzPort > 0 {
		addr = ":" + strconv.Itoa(*healthzPort)
	} else {
		addr = *httpEndpoint
	}

	if *connectionTimeout != 0 {
		klog.Warning("--connection-timeout is deprecated and will have no effect")
	}

	// Unused metrics manager, necessary for connection.Connect below
	cmm := metrics.NewCSIMetricsManagerForSidecar("")

	// Once https://github.com/container-storage-interface/spec/issues/159 is
	// resolved, if plugin does not support PUBLISH_UNPUBLISH_VOLUME, then we
	// can skip adding mapping to "csi.volume.kubernetes.io/nodeid" annotation.

	klog.V(1).Infof("Attempting to open a gRPC connection with: %q", *csiAddress)
	csiConn, err := connection.Connect(*csiAddress, cmm)
	if err != nil {
		klog.Errorf("error connecting to CSI driver: %v", err)
		os.Exit(1)
	}

	klog.V(1).Infof("Calling CSI driver to discover driver name")
	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()

	csiDriverName, err := csirpc.GetDriverName(ctx, csiConn)
	if err != nil {
		klog.Errorf("error retreiving CSI driver name: %v", err)
		os.Exit(1)
	}

	klog.V(2).Infof("CSI driver name: %q", csiDriverName)
	cmm.SetDriverName(csiDriverName)

	// Run forever
	nodeRegister(csiDriverName, addr)
}
