package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/internal/controller"
	"github.com/AnthonyPoschen/plexus-controller/pkg/runtimeapi"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	contract, err := runtimeapi.Load()
	if err != nil {
		setupLog.Error(err, "invalid runtime API contract")
		os.Exit(1)
	}
	if err := plexusv1alpha1.AddGroupToScheme(contract.Group)(scheme); err != nil {
		setupLog.Error(err, "unable to register runtime API group")
		os.Exit(1)
	}

	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, runtimeManagerOptions(contract, metricsAddr, probeAddr, enableLeaderElection))
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	kubernetesClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	if err := (&controller.GameServerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GameServer")
		os.Exit(1)
	}
	if err := (&controller.SaveExportReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		ExporterImage: os.Getenv("PLEXUS_SAVE_EXPORTER_IMAGE"),
		Progress:      controller.NewPodLogProgressReader(kubernetesClient.CoreV1()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SaveExport")
		os.Exit(1)
	}
	if err := (&controller.SaveImportReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		ImporterImage: os.Getenv("PLEXUS_SAVE_IMPORTER_IMAGE"),
		Progress:      controller.NewPodLogProgressReaderFor(kubernetesClient.CoreV1(), "save-importer"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SaveImport")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("gameserver-crd", gameServerCRDReadyz(mgr.GetAPIReader(), contract)); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "apiGroup", contract.Group, "namespace", contract.Namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func runtimeManagerOptions(contract runtimeapi.Contract, metricsAddr string, probeAddr string, enableLeaderElection bool) ctrl.Options {
	return ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "plexus-controller.plexus.gg",
		LeaderElectionNamespace: contract.Namespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				contract.Namespace: {},
			},
		},
	}
}

func gameServerCRDReadyz(reader client.Reader, contract runtimeapi.Contract) healthz.Checker {
	return func(req *http.Request) error {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := reader.Get(req.Context(), types.NamespacedName{Name: contract.GameServerCRDName()}, &crd); err != nil {
			return fmt.Errorf("GameServer CRD %s: %w", contract.GameServerCRDName(), err)
		}
		document := runtimeapi.CRDDocument{
			Metadata: runtimeapi.CRDMetadata{Name: crd.Name},
			Spec:     runtimeapi.CRDSpec{Group: crd.Spec.Group},
		}
		for _, version := range crd.Spec.Versions {
			document.Spec.Versions = append(document.Spec.Versions, runtimeapi.CRDVersion{Name: version.Name, Served: version.Served})
		}
		return contract.CheckGameServerCRD(document)
	}
}
