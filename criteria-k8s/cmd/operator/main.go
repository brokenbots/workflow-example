package main

import (
	"flag"
	"os"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"k8s.io/apimachinery/pkg/runtime"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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

	reconciler := &controller.CriteriaRunReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Config: cfg,
		Reader: &controller.PodExecReader{Config: cfg},
		Defaults: jobbuilder.Defaults{
			Image:           *defaultImage,
			DataPVC:         *dataPVC,
			ProviderBaseURL: *providerBaseURL,
		},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "setting up CriteriaRun reconciler")
		os.Exit(1)
	}

	logger.Info("starting criteria-k8s operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
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
