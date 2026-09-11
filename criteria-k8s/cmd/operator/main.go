package main

import (
	"flag"
	"os"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	metricsAddr     = flag.String("metrics-bind-address", ":8080", "Address for metrics endpoint")
	probeAddr       = flag.String("health-probe-bind-address", ":8081", "Address for health probe endpoint")
	defaultImage    = flag.String("default-image", getenv("DEFAULT_CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	dataPVC         = flag.String("data-pvc", getenv("CRITERIA_DATA_PVC", "criteria-data"), "PVC mounted at /data")
	providerBaseURL = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	development     = flag.Bool("development", false, "Enable development logging")
)

func main() {
	flag.Parse()
	logger := zap.New(zap.UseDevMode(*development))
	log.SetLogger(logger)

	ctx := ctrl.SetupSignalHandler()

	cfg, err := config.GetConfig()
	if err != nil {
		logger.Error(err, "loading kubeconfig")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering CriteriaRun scheme")
		os.Exit(1)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering batch scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering core scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		logger.Error(err, "creating manager")
		os.Exit(1)
	}

	queue := controller.NewRunQueue()

	reconciler := &controller.CriteriaRunReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Config: cfg,
		Reader: &controller.FileEventsReader{DataRoot: "/data"},
		Defaults: jobbuilder.Defaults{
			Image:           *defaultImage,
			DataPVC:         *dataPVC,
			ProviderBaseURL: *providerBaseURL,
		},
		Queue: queue,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "setting up CriteriaRun reconciler")
		os.Exit(1)
	}

	// Recover queue state from existing active runs before starting the manager
	// so in-flight runs are not lost across operator restarts.
	directClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "creating direct client for queue recovery")
		os.Exit(1)
	}
	if err := queue.Recover(ctx, directClient); err != nil {
		logger.Error(err, "recovering queue state")
		os.Exit(1)
	}

	logger.Info("starting criteria-k8s operator")
	if err := mgr.Start(ctx); err != nil {
		logger.Error(err, "operator exited")
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
