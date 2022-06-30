/*
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
	"fmt"
	"net/http"
	"net/http/pprof"

	"github.com/go-logr/zapr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/flowcontrol"
	"knative.dev/pkg/configmap/informer"
	knativeinjection "knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/signals"
	"knative.dev/pkg/system"
	controllerruntime "sigs.k8s.io/controller-runtime"

	"github.com/aws/karpenter/pkg/apis"
	"github.com/aws/karpenter/pkg/cloudprovider"
	cloudprovidermetrics "github.com/aws/karpenter/pkg/cloudprovider/metrics"
	"github.com/aws/karpenter/pkg/cloudprovider/registry"
	"github.com/aws/karpenter/pkg/config"
	"github.com/aws/karpenter/pkg/controllers"
	"github.com/aws/karpenter/pkg/controllers/counter"
	metricsnode "github.com/aws/karpenter/pkg/controllers/metrics/node"
	metricspod "github.com/aws/karpenter/pkg/controllers/metrics/pod"
	metricsprovisioner "github.com/aws/karpenter/pkg/controllers/metrics/provisioner"
	"github.com/aws/karpenter/pkg/controllers/node"
	"github.com/aws/karpenter/pkg/controllers/provisioning"
	"github.com/aws/karpenter/pkg/controllers/state"
	"github.com/aws/karpenter/pkg/controllers/termination"
	"github.com/aws/karpenter/pkg/events"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/options"
	"github.com/aws/karpenter/pkg/utils/project"
)

var (
	scheme    = runtime.NewScheme()
	opts      = options.New().MustParse()
	component = "controller"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apis.AddToScheme(scheme))
}

const appName = "karpenter"

func main() {
	controllerRuntimeConfig := controllerruntime.GetConfigOrDie()
	controllerRuntimeConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(opts.KubeClientQPS), opts.KubeClientBurst)
	controllerRuntimeConfig.UserAgent = appName
	clientSet := kubernetes.NewForConfigOrDie(controllerRuntimeConfig)

	cmw := informer.NewInformedWatcher(clientSet, system.Namespace())
	// Set up logger and watch for changes to log level
	ctx := LoggingContextOrDie(controllerRuntimeConfig, cmw)
	ctx = injection.WithConfig(ctx, controllerRuntimeConfig)
	ctx = injection.WithOptions(ctx, opts)

	logging.FromContext(ctx).Infof("Initializing with version %s", project.Version)
	// Set up controller runtime controller
	manager := controllers.NewManagerOrDie(ctx, controllerRuntimeConfig, controllerruntime.Options{
		Logger:                     zapr.NewLogger(logging.FromContext(ctx).Desugar()),
		LeaderElection:             true,
		LeaderElectionID:           "karpenter-leader-election",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		Scheme:                     scheme,
		MetricsBindAddress:         fmt.Sprintf(":%d", opts.MetricsPort),
		HealthProbeBindAddress:     fmt.Sprintf(":%d", opts.HealthProbePort),
	})

	if opts.EnableProfiling {
		utilruntime.Must(registerPprof(manager))
	}

	cloudProvider := registry.NewCloudProvider(ctx, cloudprovider.Options{ClientSet: clientSet, KubeClient: manager.GetClient(), StartAsync: manager.Elected()})
	cloudProvider = cloudprovidermetrics.Decorate(cloudProvider)

	cfg, err := config.New(ctx, clientSet, cmw)
	if err != nil {
		// this does not happen if the config map is missing or invalid, only if some other error occurs
		logging.FromContext(ctx).Fatalf("unable to load config, %s", err)
	}

	if err := cmw.Start(ctx.Done()); err != nil {
		logging.FromContext(ctx).Errorf("watching configmaps, config changes won't be applied immediately, %s", err)
	}

	recorder := events.NewDedupeRecorder(events.NewRecorder(manager.GetEventRecorderFor(appName)))
	cluster := state.NewCluster(manager.GetClient(), cloudProvider)

	if err := manager.RegisterControllers(ctx,
		provisioning.NewController(ctx, cfg, manager.GetClient(), clientSet.CoreV1(), recorder, cloudProvider, cluster),
		state.NewNodeController(manager.GetClient(), cluster),
		state.NewPodController(manager.GetClient(), cluster),
		termination.NewController(ctx, manager.GetClient(), clientSet.CoreV1(), cloudProvider),
		node.NewController(manager.GetClient(), cloudProvider),
		metricspod.NewController(manager.GetClient()),
		metricsnode.NewController(manager.GetClient()),
		metricsprovisioner.NewController(manager.GetClient()),
		counter.NewController(manager.GetClient(), cluster),
	).Start(ctx); err != nil {
		panic(fmt.Sprintf("Unable to start manager, %s", err))
	}
}

func registerPprof(manager controllers.Manager) error {
	for path, handler := range map[string]http.Handler{
		"/debug/pprof/":             http.HandlerFunc(pprof.Index),
		"/debug/pprof/cmdline":      http.HandlerFunc(pprof.Cmdline),
		"/debug/pprof/profile":      http.HandlerFunc(pprof.Profile),
		"/debug/pprof/symbol":       http.HandlerFunc(pprof.Symbol),
		"/debug/pprof/trace":        http.HandlerFunc(pprof.Trace),
		"/debug/pprof/allocs":       pprof.Handler("allocs"),
		"/debug/pprof/heap":         pprof.Handler("heap"),
		"/debug/pprof/block":        pprof.Handler("block"),
		"/debug/pprof/goroutine":    pprof.Handler("goroutine"),
		"/debug/pprof/threadcreate": pprof.Handler("threadcreate"),
	} {
		err := manager.AddMetricsExtraHandler(path, handler)
		if err != nil {
			return err
		}
	}
	return nil
}

// LoggingContextOrDie injects a logger into the returned context. The logger is
// configured by the ConfigMap `config-logging` and live updates the level.
func LoggingContextOrDie(config *rest.Config, cmw *informer.InformedWatcher) context.Context {
	ctx, startinformers := knativeinjection.EnableInjectionOrDie(signals.NewContext(), config)
	logger, atomicLevel := sharedmain.SetupLoggerOrDie(ctx, component)
	ctx = logging.WithLogger(ctx, logger)
	rest.SetDefaultWarningHandler(&logging.WarningHandler{Logger: logger})
	sharedmain.WatchLoggingConfigOrDie(ctx, cmw, logger, atomicLevel, component)
	startinformers()
	return ctx
}