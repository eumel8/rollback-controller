package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type RollbackController struct {
	client.Client
	log                logr.Logger
	GitlabToken        string
	GitlabProjectID    string
	GitlabBaseURL      string
	RevertBranchPrefix string
	DebounceSeconds    int
	pendingSHAs        map[string]time.Time // SHA -> time first seen failing
	completedSHAs      map[string]bool      // SHAs that already triggered a revert
}

func NewRollbackController(c client.Client, log logr.Logger, token, projectID, baseURL, branchPrefix string, debounce int) *RollbackController {
	return &RollbackController{
		Client:             c,
		log:                log,
		GitlabToken:        token,
		GitlabProjectID:    projectID,
		GitlabBaseURL:      baseURL,
		RevertBranchPrefix: branchPrefix,
		DebounceSeconds:    debounce,
		pendingSHAs:        make(map[string]time.Time),
		completedSHAs:      make(map[string]bool),
	}
}

// handleResource evaluates the resource state and returns how long to wait
// before re-checking (0 = no requeue needed).
func (r *RollbackController) handleResource(kind, name, namespace, sha string, ready bool) time.Duration {
	if sha == "" {
		r.log.Info("WARNING: Cannot create revert without sha", "kind", kind, "namespace", namespace, "name", name, "debounceSeconds", r.DebounceSeconds, "sha", sha)
		return 0
	}
	if !ready {
		if r.completedSHAs[sha] {
			return 0 // already triggered a revert for this SHA
		}
		if t, ok := r.pendingSHAs[sha]; ok {
			elapsed := time.Since(t)
			debounce := time.Duration(r.DebounceSeconds) * time.Second
			if elapsed >= debounce {
				r.log.Info("Failure stable, creating revert", "kind", kind, "namespace", namespace, "name", name, "debounceSeconds", r.DebounceSeconds, "sha", sha)
				r.createGitlabRevertMR(sha)
				r.completedSHAs[sha] = true
				delete(r.pendingSHAs, sha)
				return 0
			}
			// Still within debounce window — requeue when it expires.
			return debounce - elapsed
		}
		r.log.Info("Failure detected", "kind", kind, "namespace", namespace, "name", name, "sha", sha, "debounceSeconds", r.DebounceSeconds)
		r.pendingSHAs[sha] = time.Now()
		return time.Duration(r.DebounceSeconds) * time.Second
	}
	// Resource is healthy again: clear any pending tracking.
	delete(r.pendingSHAs, sha)
	return 0
}

func (r *RollbackController) createGitlabRevertMR(badSHA string) {
	branch := fmt.Sprintf("%s-%s", r.RevertBranchPrefix, badSHA)
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits/%s/revert",
		r.GitlabBaseURL, r.GitlabProjectID, badSHA)
	if os.Getenv("REVERT_MODE") == "echo" {
		r.log.Info("ECHO: would POST revert", "url", url, "branch", branch)
		return
	}
	data := fmt.Sprintf(`{"branch":"%s"}`, branch)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(data)))
	if err != nil {
		r.log.Error(err, "failed to create request")
		return
	}
	req.Header.Set("PRIVATE-TOKEN", r.GitlabToken)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.log.Error(err, "GitLab revert failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		r.log.Info("Revert commit created successfully", "sha", badSHA)
	} else {
		r.log.Error(nil, "GitLab API error", "status", resp.Status, "sha", badSHA)
	}
}

func main() {
	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	_ = kustomizev1.AddToScheme(scheme)
	_ = helmv2.AddToScheme(scheme)

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		panic(err)
	}

	token := os.Getenv("GITLAB_TOKEN")
	projectID := os.Getenv("GITLAB_PROJECT_ID")
	baseURL := os.Getenv("GITLAB_URL")
	if baseURL == "" {
		baseURL = "https://gitlab"
	}
	branchPrefix := os.Getenv("REVERT_BRANCH_PREFIX")
	if branchPrefix == "" {
		branchPrefix = "revert"
	}
	debounce := 300
	if d := os.Getenv("DEBOUNCE_SECONDS"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			debounce = n
		}
	}

	log := ctrl.Log.WithName("rollback-controller")
	rollback := NewRollbackController(mgr.GetClient(), log, token, projectID, baseURL, branchPrefix, debounce)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&kustomizev1.Kustomization{}).
		Watches(&helmv2.HelmRelease{}, &handler.EnqueueRequestForObject{}).
		Complete(&GenericReconciler{rollback}); err != nil {
		panic(err)
	}

	log.Info("Starting Rollback Controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		panic(err)
	}
}

type GenericReconciler struct {
	rollback *RollbackController
}

func (r *GenericReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Try Kustomization first
	var ks kustomizev1.Kustomization
	if err := r.rollback.Get(ctx, req.NamespacedName, &ks); err == nil {
		ready := true
		// LastAttemptedRevision is populated when the source resolves (even on apply
		// failure); fall back to LastAppliedRevision only if the former is empty.
		sha := ks.Status.LastAttemptedRevision
		if sha == "" {
			sha = ks.Status.LastAppliedRevision
		}
		for _, c := range ks.Status.Conditions {
			if c.Type == "Ready" && c.Status == "False" {
				ready = false
			}
		}
		requeue := r.rollback.handleResource("Kustomization", ks.Name, ks.Namespace, sha, ready)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	// Try HelmRelease
	var hr helmv2.HelmRelease
	if err := r.rollback.Get(ctx, req.NamespacedName, &hr); err == nil {
		ready := true
		sha := hr.Status.LastAttemptedRevision
		for _, c := range hr.Status.Conditions {
			if c.Type == "Ready" && c.Status == "False" {
				ready = false
			}
		}
		requeue := r.rollback.handleResource("HelmRelease", hr.Name, hr.Namespace, sha, ready)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	return ctrl.Result{}, nil
}
