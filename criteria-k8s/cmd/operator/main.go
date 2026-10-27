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

	criteria "github.com/brokenbots/criteria/sdk"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/proxy"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
	"github.com/go-logr/logr"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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
	// cluster. (KB-304: the second built-in legacy volume went with its
	// retired SecretProviderClass.)
	legacySecretVolumes = flag.Bool("legacy-secret-volumes", getBool(jobbuilder.EnvLegacySecretVolumes, false), "Mount the built-in copilot-spc CSI secret volume on runs without a stamped workflow (requires the OpenBao Secrets Store CSI driver)")
	dataPVC             = flag.String("data-pvc", getenv("CRITERIA_DATA_PVC", "criteria-data"), "PVC mounted at /data")
	providerBaseURL     = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	castleAddr          = flag.String("castle-addr", getenv("CASTLE_ADDR", ""), "Castle control plane Connect endpoint; empty disables run observation (read-only)")
	backfillRunArchives = flag.Bool("backfill-run-archives", getBool("BACKFILL_RUN_ARCHIVES", false), "One-shot run-archive backfill (KB-314): archive every terminal CriteriaRun into castle's run archive, then exit; the operator mode flag runs the normal reconciler loop instead")
	castleToken         = castleTokenFromEnv()
	// KB-319 shadow-run window: while open, an observational consumer
	// watches castle's Control-stream WorkflowAssignments and logs decision
	// divergence against the watcher's CriteriaRuns without creating or
	// admitting anything (the watcher keeps deciding; castle dispatch also
	// decides; the divergence log reconciles the two). See internal/control.
	shadowRunWatchdog = flag.Bool("shadow-run-watchdog", getBool("SHADOW_RUN_WATCHDOG", true), "Shadow-run window kill switch (KB-319): log assignment-driven run decisions vs the watcher's runs; the lane never writes. DEFAULTS ON — the only writer-less lane; set --shadow-run-watchdog=0 to close the window entirely")
	shadowCastleAddr  = flag.String("shadow-castle-addr", getenv("SHADOW_CASTLE_ADDR", ""), "Castle Connect endpoint for the shadow-run assignment consumer (KB-319); empty rides --castle-addr")
	shadowCriteriaID  = flag.String("shadow-criteria-id", getenv("SHADOW_CRITERIA_ID", "criteria-k8s-operator"), "Criteria id presented on the shadow-run Control subscribe (KB-319)")
	shadowRoutesFile  = flag.String("shadow-routes-file", getenv("SHADOW_ROUTES_FILE", ""), "Routes payload file for the shadow-run spec mirror (KB-319); one startup read; empty keeps the watchdog decision-only")
	// KB-338 assignment agent lane: the operator's Control consumer. With
	// the flag on, the lane connects as the project's executing agent:
	// assignment deliveries translate fail-closed onto the shared stamp
	// path (RunBuilder), the bridge mirrors CR lifecycle into castle's
	// desired-run rows, and everything serializes to 1 run per replica.
	// DEFAULT OFF — flipping it is the deliberate dispatch cutover
	// posture; the lane never starts without it (no env-only default).
	assignmentRun   = flag.Bool("assignment-agent", getBool("CRITERIA_ASSIGNMENT_AGENT", false), "Run the assignment agent lane (KB-338): operator consumes its project's castle WorkflowAssignments and stamps CriteriaRuns through the shared stamp path; default off (kill switch)")
	agentCastleAddr = flag.String("assignment-castle-addr", getenv("CRITERIA_AGENT_CASTLE_ADDR", ""), "Castle Connect endpoint for the assignment agent lane (KB-338); empty rides --castle-addr")
	agentCriteriaID = flag.String("assignment-criteria-id", getenv("CRITERIA_AGENT_CRITERIA_ID", ""), "Criteria overseer id for a pre-provisioned agent identity (KB-338); required with an agent token, ignored by the per-boot Register fallback")
	agentProject    = flag.String("assignment-project", getenv("CRITERIA_AGENT_PROJECT", ""), "Project scope the assignment agent executes for (KB-338): assignments whose project routing label names another project are never executed here; empty accepts everything dispatched to this identity")
	agentRoutesFile = flag.String("assignment-routes-file", getenv("CRITERIA_AGENT_ROUTES_FILE", ""), "Routes payload file the assignment agent lane stamps against (KB-338); empty rides --shadow-routes-file; neither set keeps the lane off (it cannot attribute runs without one)")
	debugEventsFile = flag.String("debug-events-file", getenv("CRITERIA_DEBUG_EVENTS_FILE", ""), "Debug-only events.ndjson path handed to runner Jobs via EVENTS_FILE so criteria additionally mirrors lifecycle events to a file; empty (default) runs castle-only with no events.ndjson written")
	retentionPeriod = flag.Duration("retention-period", getDuration("RETENTION_PERIOD", 7*24*time.Hour), "Keep per-ticket intake/triage artifacts this long after the last write (0 disables sweeping)")
	sweepInterval   = flag.Duration("retention-interval", getDuration("RETENTION_INTERVAL", time.Hour), "How often the retention sweep runs")
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
	// KB-344: the run-events endpoint runner Jobs receive as CASTLE_ADDR —
	// the operator's run-event proxy on the boot gate server (the criteria
	// protocol), never castle. Empty derives
	// http://criteria-k8s-operator.<ns>.svc:<port> from the boot gate
	// address; "empty" (proxy lane off) keeps runners in local file mode.
	runEventsAddr = flag.String("run-events-addr", getenv("CRITERIA_RUN_EVENTS_ADDR", ""), "Run-event proxy endpoint for runner Jobs (KB-344); empty derives the operator svc from the boot gate listener")
	runEventsSvc  = flag.String("run-events-svc", getenv("CRITERIA_RUN_EVENTS_SVC", "criteria-k8s-operator"), "Operator Service name the derived run-events endpoint uses (KB-344)")
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

	// KB-344: the run-event proxy identity is resolved once and shared by
	// the proxy and the assignment agent lane. The assignment agent's
	// credential is preferred (owner-consistent: the proxy re-emits the
	// dispatch lane's envelopes under the run's owning agent identity);
	// the shared castle token is the fallback serving the watcher lane
	// (the proxy mints the castle run itself and stamps the CR). A token
	// file read error keeps the loud log, never the secret material.
	proxyToken, proxyTokenErr := agentCastleTokenFromEnv()
	if proxyTokenErr != nil {
		logger.Error(proxyTokenErr, "run-event proxy: agent credential file unreadable; falling back to the shared CASTLE_TOKEN")
		proxyToken = castleTokenFromEnv()
	}
	proxyAddr := agentRunAddr()
	// The proxy lane is served only when it has everything: a boot gate
	// listener (it rides the same HTTP server), a castle endpoint to
	// re-emit envelopes to, and a credential to present (KB-344 review
	// remediation: any partial configuration must keep the endpoint empty,
	// because handing runner Jobs the operator endpoint while the proxy
	// cannot forward re-creates the empty-transcript dead-end this card
	// removes — without CASTLE_ADDR the runners keep their pre-KB-344
	// anonymous-to-castle posture instead). The disabling reason is logged
	// so a lane-off never stays silent.
	runEventsEndpoint, runEventsDisabled := resolveRunEventsLane(*bootgateViewAddr, *runEventsAddr, *runEventsSvc, *namespaceFlag, proxyAddr, proxyToken)
	if runEventsEndpoint != "" {
		logger.Info("run-event proxy endpoint derived (KB-344)", "endpoint", runEventsEndpoint, "bootgate", *bootgateViewAddr, "namespace", *namespaceFlag)
	} else {
		logger.Info("run-event proxy disabled (KB-344)", "reason", runEventsDisabled)
	}
	// KB-344 review remediation: build (not serve) the run-event proxy
	// before the reconciler — the reconciler enrolls runs through it
	// (Enroll stamps the per-run bootstrap credential on the CR) before
	// their runner Jobs render, so the proxy object must exist regardless
	// of which leg consumes it first. Serving still happens only on the
	// boot gate server, only when the lane is on; with the lane off the
	// reconciler renders runners without the proxy endpoint entirely.
	var runProxy *proxy.Proxy
	if runEventsEndpoint != "" {
		proxyFeed := castle.NewAssignmentEventFeed(castle.Config{Addr: proxyAddr, Token: proxyToken}, nil)
		runProxy = proxy.New(proxy.Config{
			Log:    logger,
			Client: directClient,
			NS:     *namespaceFlag,
			Submit: proxyFeed,
			Create: proxyFeed,
		})
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
		// KB-344 review remediation: the run-event proxy's enrollment
		// edge — enrolls each run (per-run bootstrap credential stamped on
		// the CR) before its runner Job renders. Nil when the proxy lane
		// is off.
		RunEvents: runProxy,
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
			Image:           *defaultImage,
			DataPVC:         *dataPVC,
			ProviderBaseURL: *providerBaseURL,
			// KB-344: the run-event proxy endpoint (the operator svc), or
			// empty when the proxy lane is off — jobbuilder then renders
			// runner Jobs without CASTLE_ADDR and the engine falls back to
			// its local events file.
			CriteriaServerAddr:  runEventsEndpoint,
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
	// KB-344: the same server also hosts the run-event proxy over the
	// criteria Connect protocol — runner engines dial this endpoint (the
	// CASTLE_ADDR they receive) and the proxy re-emits their envelopes to
	// castle under the run's owning agent identity. h2c keeps Connect
	// HTTP/2 (bidi streaming) working behind the plain-TCP listener.
	if *bootgateViewAddr != "" {
		mux := http.NewServeMux()
		view := &controller.BootgateView{Client: directClient, Namespace: *namespaceFlag}
		mux.Handle("/operator-view", view)
		if runEventsEndpoint != "" {
			prefix, handler := criteria.NewServiceHandler(runProxy)
			mux.Handle(prefix, handler)
			logger.Info("run-event proxy enabled (KB-344)", "route", prefix, "castle", proxyAddr, "namespace", *namespaceFlag)
		}
		bgSrv := &http.Server{Addr: *bootgateViewAddr, Handler: h2c.NewHandler(mux, &http2.Server{}), ReadHeaderTimeout: 5 * time.Second}
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

	// KB-319 shadow-run window: while open, an observational consumer
	// watches castle's Control-stream WorkflowAssignments and logs whether
	// the dispatch lane's decision matches the watcher's run. It decides
	// nothing and emits nothing: no CR, no admission, no lease — castle's
	// lease path skips a never-online identity and this lane never
	// heartbeats (NewControlClient's lease-safety posture). Divergence
	// rides the scrapeable "assignment divergence" Error log; the kill
	// switch (--shadow-run-watchdog=0) closes the window entirely. The
	// token resolves from SHADOW_CASTLE_TOKEN / SHADOW_CASTLE_TOKEN_FILE
	// (falling back to the shared CASTLE_TOKEN): Control is agent-owned, so
	// an orchestrator-only token cannot subscribe — castle rejects it once
	// and the consumer permanently disables itself with a loud log naming
	// the fix.
	//
	// Run step events are proxied, not re-consumed (KB-319 "proxy
	// principle"): castle streams step envelopes to the operator's run
	// observation lane (internal/castle — SubmitEvents consumption,
	// drainedSteps, Observe stamping obs.Steps), and the reconcile mirrors
	// those observed steps into the CriteriaRun's terminal archive
	// (internal/controller/terminalarchive.go, iterating obs.Steps). The
	// operator never subscribes to raw step events from a second lane; the
	// assignment consumer stays decision- and spec-comparison only.
	if !*shadowRunWatchdog {
		logger.Info("shadow-run watchdog disabled (kill switch)")
	} else if shadowAddr := shadowRunAddr(); shadowAddr == "" {
		logger.Info("shadow-run watchdog disabled (no castle address)")
	} else {
		shadowToken, err := shadowCastleTokenFromEnv()
		if err != nil {
			// Cause + path only (never the token body): a silently
			// missing file would make the lane's disable undiagnosable.
			logger.Error(err, "shadow-run watchdog: token file unreadable; falling back to the shared CASTLE_TOKEN")
			shadowToken = castleTokenFromEnv()
		}
		shadowCfg := control.Config{
			Addr:       shadowAddr,
			Token:      shadowToken,
			CriteriaID: *shadowCriteriaID,
		}
		watchdog := &control.Watchdog{
			Client:    directClient,
			Namespace: *namespaceFlag,
		}
		// KB-319 rem1 spec mirror: with a routes payload, the watchdog also
		// derives the would-be assignment-driven CriteriaRun in memory
		// (shared runspec stamping, no API writes) and compares it
		// field-for-field against the watcher's CR — spec-level divergence
		// detection on top of the decision-level compare. The routes read
		// failure degrades to decision-only rather than crashing the
		// operator (the mirror is observational; never a boot blocker).
		routesPayload, routesErr := loadShadowRoutes(*shadowRoutesFile)
		if routesErr != nil {
			logger.Error(routesErr, "shadow-run watchdog: routes payload unreadable; the mirror degrades to decision-only",
				"routesFile", *shadowRoutesFile)
		}
		watchdog.Builder = newShadowRunBuilder(routesPayload, shadowRunnerDefaults(), *namespaceFlag, directClient, logger)
		if watchdog.Builder == nil && routesErr == nil {
			logger.Info("shadow-run spec mirror disabled: no routes payload configured (--shadow-routes-file empty); the watchdog stays decision-only",
				"routesFile", *shadowRoutesFile)
		}
		stream := control.StartStream(shadowCfg, watchdog, logger)
		go func() {
			if err := stream.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error(err, "shadow-run watchdog exited")
			}
		}()
		logger.Info("shadow-run watchdog enabled (observational)", "addr", shadowAddr, "criteriaId", *shadowCriteriaID)
	}

	// KB-338 P0.2: the assignment agent lane — the operator's Control
	// consumer for its project's dispatch. With the kill switch on, the
	// lane registers (or resumes a pre-provisioned agent identity), keeps
	// its overseer online, consumes the project's WorkflowAssignments, and
	// stamps their CriteriaRuns through the SAME shared path the shadow
	// mirror derives with — 1 run per replica, the bridge mirroring CR
	// lifecycle back into the desired-run rows
	// (internal/control/agentbridge.go). Castle's lease gate hands this
	// agent its project's work only while it stays online; the
	// GetAssignmentDisposition courtesy gate re-checks ownership before
	// any stamp (fail-open: castle's per-envelope ownership check is the
	// enforcement). Kill switch: --assignment-agent=0 (the default).
	if !*assignmentRun {
		logger.Info("assignment agent lane disabled (kill switch)")
	} else if agentAddr := agentRunAddr(); agentAddr == "" {
		logger.Info("assignment agent lane disabled (no castle address)")
	} else {
		// The proxy lane resolved this token earlier (KB-344): one shared
		// resolution keeps the proxy's re-emit identity and the lane's own
		// Control identity identical.
		agentToken := proxyToken
		agentRoutesPath := resolveAgentRoutesPath(*agentRoutesFile, *shadowRoutesFile)
		agentRoutesPayload, agentRoutesErr := loadShadowRoutes(agentRoutesPath)
		if agentRoutesPayload == nil && agentRoutesErr == nil {
			agentRoutesErr = fmt.Errorf("no routes payload configured: set --assignment-routes-file (or --shadow-routes-file); the lane cannot attribute runs without one")
		}
		if agentRoutesPayload == nil {
			// Fail-safe at boot: a lane that cannot stamp a single run is
			// not a degraded lane, it is an inert consumer — keep the
			// operator alive, keep the lane off, name the fix in the log.
			logger.Error(agentRoutesErr, "assignment agent lane disabled: no routes payload configured; set --assignment-routes-file (or --shadow-routes-file)",
				"routesFile", agentRoutesPath)
		} else {
			agentCfg := control.AgentConfig{
				Addr:       agentAddr,
				Token:      agentToken,
				CriteriaID: *agentCriteriaID,
				Name:       getenv("CRITERIA_AGENT_NAME", "criteria-k8s-operator"),
				Project:    *agentProject,
			}
			builder := newShadowRunBuilder(agentRoutesPayload, shadowRunnerDefaults(), *namespaceFlag, directClient, logger)
			events := castle.NewAssignmentEventFeed(castle.Config{Addr: agentAddr, Token: agentToken}, nil)
			dispositions := castle.NewServerClient(castle.Config{Addr: agentAddr, Token: agentToken}, nil)
			agent := control.NewAssignmentAgent(agentCfg, builder, *namespaceFlag, directClient, events, dispositions, logger)
			go func() {
				if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
					logger.Error(err, "assignment agent lane exited")
				}
			}()
			logger.Info("assignment agent lane enabled (executing)", "addr", agentAddr, "criteriaId", *agentCriteriaID, "project", *agentProject, "routesFile", agentRoutesPath)
		}
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

// shadowRunAddr resolves the shadow-run consumer's castle endpoint: the
// dedicated flag first, the shared --castle-addr as fallback. Empty keeps
// the lane off entirely.
func shadowRunAddr() string {
	if *shadowCastleAddr != "" {
		return *shadowCastleAddr
	}
	return *castleAddr
}

// agentRunAddr resolves the assignment agent lane's castle endpoint: the
// dedicated flag first, the shared --castle-addr as fallback. Empty keeps
// the lane off entirely.
func agentRunAddr() string {
	if *agentCastleAddr != "" {
		return *agentCastleAddr
	}
	return *castleAddr
}

// resolveRunEventsLane resolves the CASTLE_ADDR runner Jobs receive
// (KB-344): the operator's run-event proxy endpoint, never castle. The
// lane is served only when every prerequisite holds — a boot gate listener
// to ride, a castle endpoint to re-emit envelopes to, and a credential to
// present — and an explicit --run-events-addr wins over deriving the svc
// endpoint from the listener's port. Anything missing yields ("", reason):
// runner Jobs then render without CASTLE_ADDR and stay in local file mode
// (the pre-KB-344 posture) instead of dialing a proxy whose forward path
// is dead — the empty-transcript dead-end this card exists to remove.
func resolveRunEventsLane(bootgateViewAddr, explicit, svcName, ns, castleAddr, castleToken string) (string, string) {
	if bootgateViewAddr == "" {
		return "", "no boot gate listener (bootgate-view-addr empty) to serve the proxy lane on"
	}
	if castleAddr == "" {
		return "", "no castle endpoint for the proxy to re-emit run events to (castle-addr empty)"
	}
	if castleToken == "" {
		return "", "no castle credential for the proxy to present (no agent credential and no shared token mounted)"
	}
	if explicit != "" {
		return explicit, ""
	}
	// The listener is host:port or :port; the svc endpoint needs the port
	// only. A portless or non-numeric listener port keeps the lane off
	// (nothing to derive from).
	_, port, ok := strings.Cut(bootgateViewAddr, ":")
	if !ok || port == "" {
		return "", "boot gate listener carries no port to derive the svc endpoint from"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "boot gate listener port is not numeric; no svc endpoint derivable"
	}
	return fmt.Sprintf("http://%s.%s.svc:%s", svcName, ns, port), ""
}

// agentCastleTokenFromEnv resolves the criteria token for the assignment
// agent lane: CRITERIA_AGENT_CASTLE_TOKEN first, else the file
// CRITERIA_AGENT_CASTLE_TOKEN_FILE points at (the chart delivers
// credential material as mounted files), falling back to the shared
// CASTLE_TOKEN / CASTLE_TOKEN_FILE resolution when neither agent var is
// wired. A token-file read error is returned to the caller so the fallback
// is diagnosable (path and cause only — never the secret material).
func agentCastleTokenFromEnv() (string, error) {
	token := getenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
	if file := getenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", ""); token == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", file, err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return castleTokenFromEnv(), nil
	}
	return token, nil
}

// configPinResolver is the KB-103 per-repo ConfigMap pinner the assignment
// and shadow builders consult at stamp time, mirroring the watcher's
// closure (runstamp.PinConfigRef) with the KB-250 no-silent-degrade
// discipline adapted to the operator lane: a failed read is logged at lane
// level (the operator has no kanboard channel to comment on) and the
// resolved name-only ref stamps — never silent, never a fabricated
// resourceVersion.
func configPinResolver(cl client.Client, log logr.Logger) runspec.CriteriaRunConfigRefProvider {
	return func(ns string, sel *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
		if cl == nil || sel == nil {
			return nil, nil
		}
		cfgRef, err := runstamp.PinConfigRef(context.Background(), cl, ns, sel)
		if err != nil {
			log.Error(err, "per-repo config ConfigMap read failed at stamping; stamping a name-only pin",
				"configMap", sel.ConfigName, "configMapNamespace", ns)
		}
		return cfgRef, err
	}
}

// newShadowRunBuilder wires the assignment lane's spec mirror. A deployment
// without a routes payload is decision-only posture, not a lane
// disagreement: the builder stays unwired (nil) so BuildRun's
// "routes payload not configured" degrade never enters the divergence log;
// the caller logs that posture once at startup (a routes-file READ ERROR
// keeps its own louder Error-level log). The KB-103 config pinner is
// always wired with the payload (KB-338): a named config slot must stamp
// through to the CR, never silently resolve to lane defaults.
func newShadowRunBuilder(routesPayload *routes.Payload, defaults runspec.Defaults, ns string, cl client.Client, log logr.Logger) *control.RunBuilder {
	if routesPayload == nil {
		return nil
	}
	return &control.RunBuilder{
		Defaults:     defaults,
		Namespace:    ns,
		Routes:       routesPayload,
		Client:       cl,
		Log:          log,
		ConfigPinner: configPinResolver(cl, log),
	}
}

// shadowRunnerDefaults mirrors the watcher's flag-resolution onto the
// operator's env: the same env keys the watcher's flags read
// (BUILD_CMD/TEST_CMD/CI_GATE_CMD/BASE_BRANCH/MAX_AGENT_VISITS/
// PROVIDER_BASE_URL, same fallback constants), so real deployment drift
// between the two deciders' environments surfaces as spec divergence —
// which is exactly what the shadow window exists to catch.
func shadowRunnerDefaults() runspec.Defaults {
	return runspec.Defaults{
		BuildCmd:        getenv("BUILD_CMD", ""),
		TestCmd:         getenv("TEST_CMD", ""),
		CIGateCmd:       getenv("CI_GATE_CMD", ""),
		BaseBranch:      getenv("BASE_BRANCH", ""),
		MaxAgentVisits:  getInt("MAX_AGENT_VISITS", 2),
		ProviderBaseURL: getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"),
	}
}

// resolveAgentRoutesPath picks the routes payload file the assignment
// agent lane stamps against: an explicit --assignment-routes-file wins;
// an empty one rides --shadow-routes-file; neither set leaves the lane
// off (it cannot attribute runs without a payload).
func resolveAgentRoutesPath(explicit, shadow string) string {
	if explicit != "" {
		return explicit
	}
	return shadow
}

// loadShadowRoutes reads and validates the routes payload the spec mirror
// resolves against. A nil/empty path returns (nil, nil) — decision-only
// posture; a real file failure returns the error for the caller's loud
// degrade log.
func loadShadowRoutes(path string) (*routes.Payload, error) {
	if path == "" {
		return nil, nil
	}
	return routes.LoadFile(path)
}

// shadowCastleTokenFromEnv resolves the criteria token for the shadow-run
// Control subscribe: SHADOW_CASTLE_TOKEN first, else the file
// SHADOW_CASTLE_TOKEN_FILE points at (the chart-p posture delivers
// credential material as mounted files), falling back to the shared
// CASTLE_TOKEN / CASTLE_TOKEN_FILE resolution when neither shadow var is
// wired. An orchestrator-only shared token cannot subscribe — the consumer
// logs the rejection once and disables itself rather than retrying forever.
// A shadow-token-file read error is returned to the caller so the fallback
// is diagnosable (path and cause only — never the secret material).
func shadowCastleTokenFromEnv() (string, error) {
	token := getenv("SHADOW_CASTLE_TOKEN", "")
	if file := getenv("SHADOW_CASTLE_TOKEN_FILE", ""); token == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", file, err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return castleTokenFromEnv(), nil
	}
	return token, nil
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
