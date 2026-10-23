package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	rest "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	metricsAddr       = flag.String("metrics-bind-address", ":8080", "Address for metrics endpoint")
	probeAddr         = flag.String("health-probe-bind-address", ":8081", "Address for health probe endpoint")
	defaultImage      = flag.String("default-image", getenv("DEFAULT_CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	criteriaBaseImage = flag.String("criteria-base-image", getenv("CRITERIA_BASE_IMAGE", jobbuilder.CriteriaBaseImageDefault), "Source-mode base image (CRI-230): minimal criteria image fetched-workflow runs execute on when the run spec carries no image")
	// CRI-214 M14: registry host and default tag for adapter image
	// references the operator composes itself — the fallback when neither
	// the workflow object's adapterImages override nor the event's
	// digest-verified image_reference resolves one.
	adapterRegistry = flag.String("adapter-registry", getenv(jobbuilder.EnvAdapterRegistry, jobbuilder.DefaultAdapterRegistry), "Adapter registry host for fallback adapter image references")
	adapterTag      = flag.String("adapter-tag", getenv(jobbuilder.EnvAdapterTag, jobbuilder.DefaultAdapterTag), "Default tag for adapter image references the operator composes")
	// Node arch for child Job/pod templates (runner + per-scope adapters):
	// a cluster property, so it is operator config (env CRITERIA_JOB_ARCH,
	// Helm operator.jobArch), never per-workflow state.
	jobArch = flag.String("job-arch", getenv(jobbuilder.EnvJobArch, jobbuilder.JobArchDefault), "kubernetes.io/arch nodeSelector value stamped on child Job/pod templates (cluster property)")
	// KB-3: the built-in copilot-spc secret volume a workflow-less
	// (legacy) run mounts requires the Secrets Store CSI driver and that
	// SecretProviderClass, so it is opt-in operator config. Default off
	// keeps the legacy pod mountable on clusters without OpenBao CSI; set
	// true to restore the pre-KB-3 delivery on an OpenBao-equipped
	// cluster. (KB-304: the paired linear-spc volume went with the
	// retired linear-spc SPC.)
	legacySecretVolumes = flag.Bool("legacy-secret-volumes", getBool(jobbuilder.EnvLegacySecretVolumes, false), "Mount the built-in copilot-spc CSI secret volume on runs without a stamped workflow (requires the OpenBao Secrets Store CSI driver)")
	dataPVC             = flag.String("data-pvc", getenv("CRITERIA_DATA_PVC", "criteria-data"), "PVC mounted at /data")
	providerBaseURL     = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	castleAddr          = flag.String("castle-addr", getenv("CASTLE_ADDR", ""), "Castle control plane Connect endpoint; empty disables run observation (read-only)")
	backfillRunArchives = flag.Bool("backfill-run-archives", getBool("BACKFILL_RUN_ARCHIVES", false), "One-shot run-archive backfill (KB-314): archive every terminal CriteriaRun into castle's run archive, then exit; the operator mode flag runs the normal reconciler loop instead")
	castleToken         = castleTokenFromEnv()
	debugEventsFile     = flag.String("debug-events-file", getenv("CRITERIA_DEBUG_EVENTS_FILE", ""), "Debug-only events.ndjson path handed to runner Jobs via EVENTS_FILE so criteria additionally mirrors lifecycle events to a file; empty (default) runs castle-only with no events.ndjson written")
	retentionPeriod     = flag.Duration("retention-period", getDuration("RETENTION_PERIOD", 7*24*time.Hour), "Keep per-ticket intake/triage artifacts this long after the last write (0 disables sweeping)")
	sweepInterval       = flag.Duration("retention-interval", getDuration("RETENTION_INTERVAL", time.Hour), "How often the retention sweep runs")
	// KB-24: the stall watchdog's step-progress window. A Running run whose
	// castle stream shows no step progress (lifecycle envelopes plus the
	// copilot adapter's agent-activity AdapterEvents; heartbeats, terminal
	// envelopes and non-activity adapter chatter never count) for this long
	// is failed with a StallWatchdog condition so CriteriaRun reflects
	// Failed instead of hanging in Running (the CRI-271 wedge signature).
	// The clock measures time since the newest activity event, not step
	// duration: healthy copilot turns emit agent events continuously (worst
	// observed CRI-277 gap 217s, gate-held permission waits ~10m), both far
	// inside this window, so shipped step budgets above it (60m copilot
	// develop/review turns, 3600s pr_review/branch_triage_agent) remain
	// reachable while a wedged session that emits only heartbeats trips the
	// watchdog. 0 disables the watchdog.
	stallWindow = flag.Duration("stall-window", getDuration("CRITERIA_STALL_WINDOW", 30*time.Minute), "Fail a run whose castle stream shows no step progress for this long (KB-24; 0 disables)")
	// KB-302: the terminal run's child-job deletion is deferred behind a
	// pending runner restart so the kubelet cannot orphan a fresh agent
	// container through background garbage collection. This window bounds
	// the deferral: past it the reap is forced even with the restart still
	// pending, so a never-settling pod cannot wedge terminal cleanup. 0
	// disables the force and defers until the runner pod settles.
	terminalReapForceAfter = flag.Duration("terminal-reap-force-after", getDuration("TERMINAL_REAP_FORCE_AFTER", controller.DefaultTerminalReapForceAfter), "Force a terminal run's deferred child-job deletion this long after the deferral (KB-302; 0 disables the force)")
	// CRI-144: reap adapter pods and legacy adapter Jobs whose owning
	// CriteriaRun is gone (force-deleted CRs orphan them; GC only covers
	// deletionTimestamp-propagated deletes). 0 disables the sweep.
	adapterSweepInterval = flag.Duration("adapter-sweep-interval", getDuration("ADAPTER_SWEEP_INTERVAL", 30*time.Second), "How often adapter pods/jobs of deleted CriteriaRuns are swept (0 disables)")
	// KB-309: archived terminal CriteriaRun retention. Deleting an archived
	// terminal run loses nothing (the castle run archive holds the history)
	// but bounds the operator's live memory of finished work. 0 days
	// disables the sweep; the keep-last floor protects the newest terminal
	// runs of a ticket family for failure forensics (0 keeps none); the
	// namespace allowlist opts namespaces in for DELETION (the sweep is
	// inert in namespaces not listed), so cutovers flip it on after their
	// archive leg verifies.
	crRetentionDays       = flag.Int("cr-retention-days", getInt("CR_RETENTION_DAYS", controller.DefaultCRRetentionDays), "Delete archived terminal CriteriaRuns older than this many days (KB-309; 0 disables)")
	crRetentionKeepLast   = flag.Int("cr-retention-keep-last", getInt("CR_RETENTION_KEEP_LAST", controller.DefaultCRRetentionKeepLast), "Keep this many newest terminal CriteriaRuns per ticket family regardless of age (KB-309; 0 keeps none)")
	crRetentionNamespaces = flag.String("cr-retention-namespaces", getenv("CR_RETENTION_NAMESPACES", ""), "Comma-separated namespaces where archived terminal CriteriaRuns get deleted (KB-309); empty keeps the sweep inert")
	namespaceFlag         = flag.String("namespace", getenv("CRITERIA_NAMESPACE", "criteria-jobs"), "Namespace the operator manages")
	// CRI-264: the operator Deployment whose env the reconciler probes for
	// the runner image resolution. A CriteriaRun admitted while this
	// Deployment rolls out must not read the reconciling pod's (stale)
	// process env: old and new operator pods both reconcile during a
	// rollout, and the old pod's env pins the stale base image into the
	// runner Job template.
	operatorDeployment = flag.String("operator-deployment", getenv("OPERATOR_DEPLOYMENT", "criteria-k8s-operator"), "Operator Deployment the reconciler probes for the live CRITERIA_BASE_IMAGE / DEFAULT_CRITERIA_IMAGE values (CRI-264)")
	development        = flag.Bool("development", false, "Enable development logging")
	// KB-234 operator leg: the boot gate's operator-view endpoint. The
	// runner's boot gate (internal/bootgate) resolves its CR's phase here
	// before registering with castle; ":8082" default, empty disables.
	bootgateViewAddr = flag.String("bootgate-view-addr", getenv("BOOTGATE_VIEW_ADDR", ":8082"), "Boot gate operator-view listen address serving GET /operator-view (KB-234; empty disables)")
)

func main() {
	flag.Parse()
	logger := zap.New(zap.UseDevMode(*development))
	log.SetLogger(logger)

	var ctx context.Context
	ctx = ctrl.SetupSignalHandler()

	cfg, err := config.GetConfig()
	if err != nil {
		logger.Error(err, "loading kubeconfig")
		os.Exit(1)
	}

	scheme, err := operatorScheme()
	if err != nil {
		logger.Error(err, "building the operator scheme")
		os.Exit(1)
	}

	// The archive lane rides the same token as run observation (KB-314):
	// castle's interceptor rejects anonymous ArchiveRun calls, so the lane
	// is only configured with address AND token — either half missing
	// disables the archive leg (the reconciler's gate then skips and
	// terminal cleanup proceeds; it never wedges terminal reaping) and the
	// backfill mode fails closed instead of pretending coverage. The
	// address-without-token variant is logged loudly here so a rollout
	// forgetting the token cannot silently leave every terminal run
	// unarchived.
	castleArchiver := castle.NewRunArchiveClient(castle.RunArchiverConfig{Addr: *castleAddr, Token: castleToken}, nil)
	if *castleAddr != "" && castleToken == "" {
		logger.Error(errors.New("castle address is set but no criteria token is wired"),
			"run-archive lane disabled: castle address is set but CASTLE_TOKEN (or the CASTLE_TOKEN_FILE CSI mount the chart's operator.castleTokenSpc renders, or the secretKeyRef in config/manager) is missing, so terminal CriteriaRuns will NOT be archived into castle — wire the token before relying on run archives",
			"castleAddr", *castleAddr)
	}

	// KB-314 backfill mode: one-shot archive of the operator's terminal
	// CriteriaRun backlog, then exit. Run on demand (an operator image
	// exec'd with --backfill-run-archives) — never before #309's retention
	// sweep deletes castle history.
	if *backfillRunArchives {
		if err := runArchiveBackfill(ctx, cfg, scheme, castleArchiver, *castleAddr, castleToken, *namespaceFlag, getenv("OPERATOR_VERSION", ""), logger); err != nil {
			logger.Error(err, "run-archive backfill failed")
			os.Exit(1)
		}
		return
	}

	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         false,
		// Scope the manager's caches and default watches to the managed
		// namespace; the operator's Role grants no cluster-scope access.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{*namespaceFlag: {}},
		},
	})
	if err != nil {
		logger.Error(err, "creating manager")
		os.Exit(1)
	}

	queue := controller.NewRunQueue()

	// Direct (uncached) client for the queue recovery read and the
	// operator-env probe: the probe must see the Deployment's currently
	// declared env, not a possibly-stale informer copy (CRI-264).
	directClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "creating direct client")
		os.Exit(1)
	}

	// Observe run lifecycle from castle (CRI-133 ServerService API) instead
	// of reading events.ndjson off the PVC. The token comes from the
	// CASTLE_TOKEN environment only so it never leaks into argv.
	castleClient := castle.New(castle.Config{Addr: *castleAddr, Token: castleToken}, nil)

	// Feed per-scope adapter pod-state observations (KB-225) back into the
	// run's castle event stream over the same adapter-event channel the
	// engine uses, so session-wait expiry names the pod's phase instead of
	// the blind 15m verdict (KB-70 PodStateProbe). Disabled entirely with
	// --castle-addr empty.
	podStateFeed := castle.NewPodStateFeed(castle.PodStateFeedConfig{Addr: *castleAddr, Token: castleToken}, nil)

	reconciler := &controller.CriteriaRunReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("criteria-k8s-operator"),
		Castle:   castleClient,
		PodState: podStateFeed,
		// The run-archive client (KB-314): stores terminal run records
		// before child-job reaping and finalizer removal.
		RunArchiver: castleArchiver,
		// Archive-record provenance: the running operator's version, from
		// OPERATOR_VERSION (unset resolves to a "dev" record value).
		OperatorVersion: getenv("OPERATOR_VERSION", ""),
		// Probe the operator Deployment's declared env so a run admitted
		// mid-rollout resolves the image the cluster deploys with instead
		// of the reconciling pod's stale process env (CRI-264).
		EnvProbe: &controller.DeploymentEnvProbe{
			Client:     directClient,
			Namespace:  *namespaceFlag,
			Deployment: *operatorDeployment,
		},
		Defaults: jobbuilder.Defaults{
			Image:               *defaultImage,
			DataPVC:             *dataPVC,
			ProviderBaseURL:     *providerBaseURL,
			CastleAddr:          *castleAddr,
			DebugEventsFile:     *debugEventsFile,
			CriteriaBaseImage:   *criteriaBaseImage,
			AdapterRegistry:     *adapterRegistry,
			AdapterTag:          *adapterTag,
			JobArch:             *jobArch,
			LegacySecretVolumes: *legacySecretVolumes,
		},
		Queue: queue,
		// KB-302: the stall watchdog fails wedged Running runs — the run
		// parks with its failure reason; recovery decisions belong to the
		// shepherd (arc_shepherd_v1), with Job backoff covering
		// in-container transients. The watcher neither refires nor
		// reparks a failed run.
		StallWindow:            *stallWindow,
		TerminalReapForceAfter: *terminalReapForceAfter,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "setting up CriteriaRun reconciler")
		os.Exit(1)
	}

	// Recover queue state from existing active runs before starting the manager
	// so in-flight runs are not lost across operator restarts.
	if err := queue.Recover(ctx, directClient, *namespaceFlag); err != nil {
		logger.Error(err, "recovering queue state")
		os.Exit(1)
	}

	// Retention sweep keeps the data PVC bounded: intake/triage dirs older
	// than the retention window are removed. Run in the background; it shares
	// the manager's lifecycle.
	if *retentionPeriod > 0 {
		sweeper := controller.NewRetentionSweeper("/data", *retentionPeriod, *sweepInterval)
		go sweeper.Run(ctx)
		logger.Info("retention sweep enabled", "period", retentionPeriod.String(), "interval", sweepInterval.String())
	} else {
		logger.Info("retention sweep disabled (retention-period=0)")
	}

	// Adapter sweep reaps adapter pods and legacy adapter Jobs whose owning
	// CriteriaRun no longer exists (CRI-144): force-deleted CRs orphan their
	// pods, and those keep dialing the dead runner's shim forever. Uses the
	// direct (uncached) client so a sweep can never act on a stale view of
	// which runs exist. Shares the manager's lifecycle.
	if *adapterSweepInterval > 0 {
		adapterSweeper := controller.NewAdapterSweeper(directClient, scheme, *namespaceFlag, *adapterSweepInterval)
		go adapterSweeper.Run(ctx)
		logger.Info("adapter sweep enabled", "interval", adapterSweepInterval.String(), "namespace", *namespaceFlag)
	} else {
		logger.Info("adapter sweep disabled (adapter-sweep-interval=0)")
	}

	// KB-309: archived terminal CriteriaRun retention. The constructor
	// sanitizes the allowlist (trims, drops blanks), so the enable decision
	// rides the sanitized set: deletion needs a window AND at least one
	// opted-in namespace (empty allowlist keeps the sweep inert). The
	// flag's default already resolves the env var (CR_RETENTION_NAMESPACES),
	// so the flag value is the single source here — reading the env a
	// second time would silently ignore the flag. The operator manages a
	// single namespace, so only its allowlist entry matters; other entries
	// are inert. Uses the DIRECT client so deletion never acts on a stale
	// view of which runs exist, and shares the manager's lifecycle.
	retentionNamespaces := parseCsv(*crRetentionNamespaces)
	crRetentionSweeper := controller.NewCRRetentionSweeper(directClient, *namespaceFlag, time.Duration(*crRetentionDays)*24*time.Hour, *crRetentionKeepLast, retentionNamespaces, *sweepInterval)
	if *crRetentionDays > 0 && len(crRetentionSweeper.Allowlist) > 0 {
		go crRetentionSweeper.Run(ctx)
		logger.Info("criteria run retention sweep enabled",
			"days", *crRetentionDays, "keepLast", *crRetentionKeepLast,
			"namespaces", retentionNamespaces, "interval", sweepInterval.String())
	} else if *crRetentionDays > 0 {
		logger.Info("criteria run retention sweep disabled (no namespaces opted in)", "days", *crRetentionDays)
	} else {
		logger.Info("criteria run retention sweep disabled (cr-retention-days=0)")
	}

	// KB-234 operator leg: serve the boot gate's operator view. The
	// endpoint reads the CR through the DIRECT client (never the cache) so
	// a boot decision never rides a stale informer view.
	if *bootgateViewAddr != "" {
		mux := http.NewServeMux()
		view := &controller.BootgateView{Client: directClient, Namespace: *namespaceFlag}
		mux.Handle("/operator-view", view)
		bgSrv := &http.Server{Addr: *bootgateViewAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			_ = bgSrv.Shutdown(context.Background())
		}()
		go func() {
			logger.Info("boot gate operator view enabled", "addr", *bootgateViewAddr)
			if err := bgSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error(err, "boot gate view server exited")
				os.Exit(1)
			}
		}()
	} else {
		logger.Info("boot gate operator view disabled (bootgate-view-addr empty)")
	}

	logger.Info("starting criteria-k8s operator")
	if err := mgr.Start(ctx); err != nil {
		logger.Error(err, "operator exited")
		os.Exit(1)
	}
}

// operatorScheme builds the runtime scheme every client in this binary is
// constructed with (manager caches, the direct client, the operator-env
// probe). Every kind any of those clients reads by value must be registered
// here: a typed Get of an unregistered kind fails before any API call with
// "no kind is registered for the type ..." (CRI-264: the missing apps/v1
// registration made the DeploymentEnvProbe's Deployment read — and with it
// the whole admission-race enforcement — dead in the shipped binary, while
// the controller suite kept passing against its own scheme).
func operatorScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering CriteriaRun scheme: %w", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering batch scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering core scheme: %w", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering apps scheme: %w", err)
	}
	return scheme, nil
}

// castleTokenFromEnv resolves the castle criteria token for the run-archive
// and observation lanes: CASTLE_TOKEN first, else the file CASTLE_TOKEN_FILE
// points at (the OpenBao Secrets Store CSI mount the chart renders — the
// chart posture delivers credential material as mounted files, never env
// values). Trailing whitespace from the mount file is trimmed. An absent or
// unreadable file yields an empty token: the lane stays disabled and the
// startup path logs the loud address-without-token error.
func castleTokenFromEnv() string {
	token := getenv("CASTLE_TOKEN", "")
	if file := getenv("CASTLE_TOKEN_FILE", ""); token == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		token = strings.TrimSpace(string(data))
	}
	return token
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// getBool resolves an operator env var to a bool flag default: the parsed
// value when it parses, else the fallback (mirrors getDuration's leniency).
func getBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// getInt resolves an operator env var to an int flag default: the parsed
// value when it parses, else the fallback (mirrors getBool's leniency).
func getInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// parseCsv splits a comma-separated allowlist into its non-blank entries
// (trimmed). The CR retention flag's default resolves the env var, so the
// flag value is the single input to this.
func parseCsv(v string) []string {
	var out []string
	for _, entry := range strings.Split(v, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// runArchiveBackfill runs the KB-314 one-shot backfill of the operator's
// terminal CriteriaRun backlog into castle's run archive, then reports
// coverage: it exits non-zero when any terminal run remains unarchived, so
// the caller (a Job over the operator image, an engineer's exec) re-runs
// it — archive stamps make re-runs skip completed work.
// newBackfillKubeClient is the kube client constructor the backfill mode
// uses; tests replace it to inject a fake client.
var newBackfillKubeClient = func(cfg *rest.Config, scheme *runtime.Scheme) (client.Client, error) {
	return client.New(cfg, client.Options{Scheme: scheme})
}

func runArchiveBackfill(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme, archiver castle.RunArchiver, addr, token, namespace, operatorVersion string, logger logr.Logger) error {
	if archiver == nil || archiver.Disabled() {
		return fmt.Errorf("run-archive backfill requires --castle-addr and CASTLE_TOKEN: archiving nothing and exiting clean would falsely claim coverage")
	}
	directClient, err := newBackfillKubeClient(cfg, scheme)
	if err != nil {
		return fmt.Errorf("building the backfill client: %w", err)
	}
	enricher := castle.New(castle.Config{Addr: addr, Token: token}, nil)
	res, err := controller.BackfillRunArchives(ctx, directClient, archiver, enricher, namespace, operatorVersion, logger)
	if err != nil {
		return err
	}
	logger.Info("run-archive backfill pass complete",
		"terminalRuns", res.TerminalRuns,
		"alreadyArchived", res.AlreadyArchived,
		"archived", res.Archived,
		"failed", res.Failed,
		"namespace", namespace)
	missing, err := controller.CountMissingRunArchives(ctx, directClient, namespace)
	if err != nil {
		return err
	}
	if missing > 0 || res.Failed > 0 {
		return fmt.Errorf("%d terminal CriteriaRuns remain without a castle archive record (%d failed this pass; re-run to resume)", missing, res.Failed)
	}
	logger.Info("run-archive backfill coverage complete: every terminal CriteriaRun carries an archive id")
	return nil
}
