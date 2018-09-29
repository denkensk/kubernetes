/*
Copyright 2014 The Kubernetes Authors.

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

// Package app implements a Server object for running the scheduler.
package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	goruntime "runtime"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	storageinformers "k8s.io/client-go/informers/storage/v1"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/leaderelection"
	schedulerserverconfig "k8s.io/kubernetes/cmd/kube-scheduler/app/config"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/algorithmprovider"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/util/configz"
	utilflag "k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/version"
	"k8s.io/kubernetes/pkg/version/verflag"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
)

// NewSchedulerCommand creates a *cobra.Command object with default parameters
func NewSchedulerCommand() *cobra.Command {
	opts, err := options.NewOptions()
	if err != nil {
		glog.Fatalf("unable to initialize command options: %v", err)
	}

	cmd := &cobra.Command{
		Use: "kube-scheduler",
		Long: `The Kubernetes scheduler is a policy-rich, topology-aware,
workload-specific function that significantly impacts availability, performance,
and capacity. The scheduler needs to take into account individual and collective
resource requirements, quality of service requirements, hardware/software/policy
constraints, affinity and anti-affinity specifications, data locality, inter-workload
interference, deadlines, and so on. Workload-specific requirements will be exposed
through the API as necessary.`,
		Run: func(cmd *cobra.Command, args []string) {
			verflag.PrintAndExitIfRequested()
			utilflag.PrintFlags(cmd.Flags())

			if len(args) != 0 {
				fmt.Fprint(os.Stderr, "arguments are not supported\n")
			}

			if errs := opts.Validate(); len(errs) > 0 {
				fmt.Fprintf(os.Stderr, "%v\n", utilerrors.NewAggregate(errs))
				os.Exit(1)
			}

			if len(opts.WriteConfigTo) > 0 {
				if err := options.WriteConfigFile(opts.WriteConfigTo, &opts.ComponentConfig); err != nil {
					fmt.Fprintf(os.Stderr, "%v\n", err)
					os.Exit(1)
				}
				glog.Infof("Wrote configuration to: %s\n", opts.WriteConfigTo)
				return
			}

			c, err := opts.Config()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

			stopCh := make(chan struct{})
			if err := Run(c.Complete(), stopCh); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
		},
	}

	opts.AddFlags(cmd.Flags())
	cmd.MarkFlagFilename("config", "yaml", "yml", "json")

	return cmd
}

// Run runs the Scheduler.
func Run(c schedulerserverconfig.CompletedConfig, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	glog.Infof("Version: %+v", version.Get())

	// Apply algorithms based on feature gates.
	// TODO: make configurable?
	algorithmprovider.ApplyFeatureGates()

	// Configz registration.
	if cz, err := configz.New("componentconfig"); err == nil {
		cz.Set(c.ComponentConfig)
	} else {
		return fmt.Errorf("unable to register configz: %s", err)
	}

	var storageClassInformer storageinformers.StorageClassInformer
	if utilfeature.DefaultFeatureGate.Enabled(features.VolumeScheduling) {
		storageClassInformer = c.InformerFactory.Storage().V1().StorageClasses()
	}

	// Create the scheduler.
	sched, err := scheduler.New(c.Client, c.InformerFactory.Core().V1().Nodes(), c.PodInformer,
		c.InformerFactory.Core().V1().PersistentVolumes(), c.InformerFactory.Core().V1().PersistentVolumeClaims(),
		c.InformerFactory.Core().V1().ReplicationControllers(), c.InformerFactory.Apps().V1().ReplicaSets(),
		c.InformerFactory.Apps().V1().StatefulSets(), c.InformerFactory.Core().V1().Services(),
		c.InformerFactory.Policy().V1beta1().PodDisruptionBudgets(), storageClassInformer, c.Recorder,
		scheduler.WithSchedulerName(c.ComponentConfig.SchedulerName), scheduler.WithHardPodAffinitySymmetricWeight(c.ComponentConfig.HardPodAffinitySymmetricWeight),
		scheduler.WithEnableEquivalenceClassCache(c.ComponentConfig.EnableContentionProfiling),
		scheduler.WithDisablePreemption(c.ComponentConfig.DisablePreemption), scheduler.WithPercentageOfNodesToScore(c.ComponentConfig.PercentageOfNodesToScore),
		scheduler.WithBindTimeoutSeconds(*c.ComponentConfig.BindTimeoutSeconds), scheduler.WithSchedulerAlgorithmSource(c.ComponentConfig.AlgorithmSource))
	if err != nil {
		return err
	}

	// Prepare the event broadcaster.
	if c.Broadcaster != nil && c.EventClient != nil {
		c.Broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.EventClient.Events("")})
	}

	// Start up the healthz server.
	if c.InsecureServing != nil {
		separateMetrics := c.InsecureMetricsServing != nil
		handler := buildHandlerChain(newHealthzHandler(&c.ComponentConfig, separateMetrics), nil, nil)
		if err := c.InsecureServing.Serve(handler, 0, stopCh); err != nil {
			return fmt.Errorf("failed to start healthz server: %v", err)
		}
	}
	if c.InsecureMetricsServing != nil {
		handler := buildHandlerChain(newMetricsHandler(&c.ComponentConfig), nil, nil)
		if err := c.InsecureMetricsServing.Serve(handler, 0, stopCh); err != nil {
			return fmt.Errorf("failed to start metrics server: %v", err)
		}
	}
	if c.SecureServing != nil {
		handler := buildHandlerChain(newHealthzHandler(&c.ComponentConfig, false), c.Authentication.Authenticator, c.Authorization.Authorizer)
		if err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			// fail early for secure handlers, removing the old error loop from above
			return fmt.Errorf("failed to start healthz server: %v", err)
		}
	}

	// Start all informers.
	go c.PodInformer.Informer().Run(stopCh)
	c.InformerFactory.Start(stopCh)

	// Wait for all caches to sync before scheduling.
	c.InformerFactory.WaitForCacheSync(stopCh)
	controller.WaitForCacheSync("scheduler", stopCh, c.PodInformer.Informer().HasSynced)

	// Prepare a reusable run function.
	run := func(ctx context.Context) {
		sched.Run()
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.TODO()) // TODO once Run() accepts a context, it should be used here
	defer cancel()

	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// If leader election is enabled, run via LeaderElector until done and exit.
	if c.LeaderElection != nil {
		c.LeaderElection.Callbacks = leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				utilruntime.HandleError(fmt.Errorf("lost master"))
			},
		}
		leaderElector, err := leaderelection.NewLeaderElector(*c.LeaderElection)
		if err != nil {
			return fmt.Errorf("couldn't create leader elector: %v", err)
		}

		leaderElector.Run(ctx)

		return fmt.Errorf("lost lease")
	}

	// Leader election is disabled, so run inline until done.
	run(ctx)
	return fmt.Errorf("finished without leader elect")
}

// buildHandlerChain wraps the given handler with the standard filters.
func buildHandlerChain(handler http.Handler, authn authenticator.Request, authz authorizer.Authorizer) http.Handler {
	requestInfoResolver := &apirequest.RequestInfoFactory{}
	failedHandler := genericapifilters.Unauthorized(legacyscheme.Codecs, false)

	handler = genericapifilters.WithRequestInfo(handler, requestInfoResolver)
	handler = genericapifilters.WithAuthorization(handler, authz, legacyscheme.Codecs)
	handler = genericapifilters.WithAuthentication(handler, authn, failedHandler)
	handler = genericapifilters.WithRequestInfo(handler, requestInfoResolver)
	handler = genericfilters.WithPanicRecovery(handler)

	return handler
}

func installMetricHandler(pathRecorderMux *mux.PathRecorderMux) {
	configz.InstallHandler(pathRecorderMux)
	defaultMetricsHandler := prometheus.Handler().ServeHTTP
	pathRecorderMux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "DELETE" {
			metrics.Reset()
			io.WriteString(w, "metrics reset\n")
			return
		}
		defaultMetricsHandler(w, req)
	})
}

// newMetricsHandler builds a metrics server from the config.
func newMetricsHandler(config *kubeschedulerconfig.KubeSchedulerConfiguration) http.Handler {
	pathRecorderMux := mux.NewPathRecorderMux("kube-scheduler")
	installMetricHandler(pathRecorderMux)
	if config.EnableProfiling {
		routes.Profiling{}.Install(pathRecorderMux)
		if config.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
	}
	return pathRecorderMux
}

// newHealthzServer creates a healthz server from the config, and will also
// embed the metrics handler if the healthz and metrics address configurations
// are the same.
func newHealthzHandler(config *kubeschedulerconfig.KubeSchedulerConfiguration, separateMetrics bool) http.Handler {
	pathRecorderMux := mux.NewPathRecorderMux("kube-scheduler")
	healthz.InstallHandler(pathRecorderMux)
	if !separateMetrics {
		installMetricHandler(pathRecorderMux)
	}
	if config.EnableProfiling {
		routes.Profiling{}.Install(pathRecorderMux)
		if config.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
	}
	return pathRecorderMux
}