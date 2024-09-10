/*
Copyright 2022.

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
	"flag"
	"fmt"
	"os"
	"time"

	"net/http"
	_ "net/http/pprof"

	"golang.org/x/oauth2/clientcredentials"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	//+kubebuilder:scaffold:imports

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cgrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/record"

	bmcv1 "github.com/pnap/cluster-api-provider-bmc/api/v1beta1"
	controllers "github.com/pnap/cluster-api-provider-bmc/controllers"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	klog.InitFlags(nil)

	_ = clientgoscheme.AddToScheme(scheme)
	_ = bmcv1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		enableLeaderElection        bool
		leaderElectionNamespace     string
		leaderElectionLeaseDuration time.Duration
		leaderElectionRenewDeadline time.Duration
		leaderElectionRetryPeriod   time.Duration

		metricsAddr  string
		probeAddr    string
		profilerAddr string
		webhookPort  int

		reconcileTimeout time.Duration
		syncPeriod       time.Duration
		watchNamespace   string
	)

	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, `leader-election-namespace`, ``, ``)
	flag.DurationVar(&leaderElectionLeaseDuration, `leader-election-lease-duration`, 15*time.Second, ``)
	flag.DurationVar(&leaderElectionRenewDeadline, `leader-election-renew-deadline`, 10*time.Second, ``)
	flag.DurationVar(&leaderElectionRetryPeriod, `leader-election-retry-period`, 5*time.Second, ``)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&profilerAddr, `profiler-address`, ``, ``)
	flag.IntVar(&webhookPort, `webhook-port`, 9443, ``)
	flag.DurationVar(&reconcileTimeout, `reconcile-timeout`, 90*time.Minute, ``)
	flag.DurationVar(&syncPeriod, `sync-period`, 10*time.Minute, ``)
	flag.StringVar(&watchNamespace, `namespace`, ``, ``)
	flag.Parse()

	// sets the ctrl package scoped logger
	ctrl.SetLogger(klogr.New())
	ctx := ctrl.SetupSignalHandler()

	// load the BMC credentials and config
	if len(os.Getenv(controllers.ENV_BMC_CLIENT_ID)) <= 0 ||
		len(os.Getenv(controllers.ENV_BMC_CLIENT_SECRET)) <= 0 ||
		len(os.Getenv(controllers.ENV_BMC_TOKEN_URL)) <= 0 ||
		len(os.Getenv(controllers.ENV_BMC_ENDPOINT_URL)) <= 0 {
		setupLog.Error(fmt.Errorf(`incomplete BMC connection configuration`), `incomplete BMC configuration`)
		os.Exit(1)
	}
	bmcConfig := clientcredentials.Config{
		ClientID:     os.Getenv(controllers.ENV_BMC_CLIENT_ID),
		ClientSecret: os.Getenv(controllers.ENV_BMC_CLIENT_SECRET),
		TokenURL:     os.Getenv(controllers.ENV_BMC_TOKEN_URL),
		Scopes:       []string{"bmc", "bmc.read"}}

	// start the profiler
	if len(profilerAddr) <= 0 {
		profilerAddr = `:6060`
	}
	go func() {
		setupLog.Error(http.ListenAndServe(profilerAddr, nil), `error serving profiler`)
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,

		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "controller-leader-election-bmc",
		LeaseDuration:    &leaderElectionLeaseDuration,
		RenewDeadline:    &leaderElectionRenewDeadline,
		RetryPeriod:      &leaderElectionRetryPeriod,

		LeaderElectionNamespace:    leaderElectionNamespace,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,

		MetricsBindAddress:     metricsAddr,
		HealthProbeBindAddress: probeAddr,
		Port:                   webhookPort,

		Namespace:  watchNamespace,
		SyncPeriod: &syncPeriod,

		EventBroadcaster: cgrecord.NewBroadcasterWithCorrelatorOptions(
			cgrecord.CorrelatorOptions{BurstSize: 100}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	record.InitFromRecorder(mgr.GetEventRecorderFor(`bmc-controller`))

	if err = (&controllers.BMCClusterReconciler{
		Client:           mgr.GetClient(),
		BMCClientConfig:  bmcConfig,
		BMCEndpointURL:   os.Getenv(controllers.ENV_BMC_ENDPOINT_URL),
		Recorder:         mgr.GetEventRecorderFor(`bmccluster-controller`),
		ReconcileTimeout: reconcileTimeout,
	}).SetupWithManager(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BMCCluster")
		os.Exit(1)
	}
	if err = (&controllers.BMCMachineReconciler{
		Client:           mgr.GetClient(),
		BMCClientConfig:  bmcConfig,
		BMCEndpointURL:   os.Getenv(controllers.ENV_BMC_ENDPOINT_URL),
		Recorder:         mgr.GetEventRecorderFor(`bmcmachine-controller`),
		ReconcileTimeout: reconcileTimeout,
	}).SetupWithManager(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BMCMachine")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
