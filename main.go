package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

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
	GitlabToken        string
	GitlabProjectID    string
	GitlabBaseURL      string
	RevertBranchPrefix string
	DebounceSeconds    int
	pendingSHAs        map[string]time.Time // SHA -> time first seen failing
	completedSHAs      map[string]bool      // SHAs that already triggered a revert
}

func NewRollbackController(c client.Client, token, projectID, baseURL, branchPrefix string, debounce int) *RollbackController {
	return &RollbackController{
		Client:             c,
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
				fmt.Printf("[%s/%s/%s] Failure stable for %ds. Creating revert for SHA %s\n",
					kind, namespace, name, r.DebounceSeconds, sha)
				r.createGitlabRevertMR(sha)
				r.completedSHAs[sha] = true
				delete(r.pendingSHAs, sha)
				return 0
			}
			// Still within debounce window — requeue when it expires.
			return debounce - elapsed
		}
		fmt.Printf("[%s/%s/%s] Failure detected for SHA %s, will revert after %ds debounce\n",
			kind, namespace, name, sha, r.DebounceSeconds)
		r.pendingSHAs[sha] = time.Now()
		return time.Duration(r.DebounceSeconds) * time.Second
	}
	// Resource is healthy again: clear any pending tracking.
	delete(r.pendingSHAs, sha)
	return 0
}

func (r *RollbackController) createGitlabRevertMR(badSHA string) {
	branch := fmt.Sprintf("%s-%s", r.RevertBranchPrefix, badSHA)
	if os.Getenv("REVERT_MODE") == "echo" {
		fmt.Printf("[ECHO] Would POST %s/api/v4/projects/%s/repository/commits/%s/revert -> branch: %s\n",
			r.GitlabBaseURL, r.GitlabProjectID, badSHA, branch)
		return
	}
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits/%s/revert",
		r.GitlabBaseURL, r.GitlabProjectID, badSHA)
	data := fmt.Sprintf(`{"branch":"%s"}`, branch)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(data)))
	if err != nil {
		fmt.Println("Failed to create request:", err)
		return
	}
	req.Header.Set("PRIVATE-TOKEN", r.GitlabToken)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Println("GitLab Revert failed:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Println("Revert commit created successfully for", badSHA)
	} else {
		fmt.Println("GitLab API returned", resp.Status)
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

	rollback := NewRollbackController(mgr.GetClient(), token, projectID, baseURL, branchPrefix, debounce)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&kustomizev1.Kustomization{}).
		Watches(&helmv2.HelmRelease{}, &handler.EnqueueRequestForObject{}).
		Complete(&GenericReconciler{rollback}); err != nil {
		panic(err)
	}

	fmt.Println("Starting Rollback Controller...")
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
