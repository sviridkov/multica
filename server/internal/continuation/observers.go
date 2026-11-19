package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// HTTPObserver uses only GET on fixed provider hosts. Tokens stay in memory
// and never enter the ledger, URL, diagnostics or returned errors.
type HTTPObserver struct {
	Client      *http.Client
	GitHubToken string
	RetellToken string
}

func (r HTTPObserver) get(ctx context.Context, url, token string, out any) error {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Never forward an authorization header through a redirect.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("invalid provider request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	response, err := copyClient.Do(req)
	if err != nil {
		return errors.New("provider read unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("provider state unconfirmed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid provider response")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid provider response")
	}
	return nil
}

func (r HTTPObserver) Observe(ctx context.Context, o Operation) (Observation, error) {
	if _, err := o.key(); err != nil {
		return Observation{}, err
	}
	switch o.Kind {
	case "merge":
		var pr struct {
			Number      int    `json:"number"`
			State       string `json:"state"`
			Merged      bool   `json:"merged"`
			MergeCommit string `json:"merge_commit_sha"`
			Head        struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				SHA  string `json:"sha"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"base"`
		}
		url := "https://api.github.com/repos/" + o.Repository
		if err := r.get(ctx, url+"/pulls/"+strconv.Itoa(o.PR), r.GitHubToken, &pr); err != nil {
			return Observation{}, err
		}
		if pr.Number != o.PR || pr.Base.Repo.FullName != o.Repository {
			return Observation{}, errors.New("provider identity mismatch")
		}
		s := Observation{Known: true, ObservedAt: time.Now().UTC(), Head: pr.Head.SHA, Base: pr.Base.SHA, Merged: pr.Merged, MergeCommit: pr.MergeCommit, Open: pr.State == "open"}
		if pr.Merged {
			if !shaPattern.MatchString(pr.MergeCommit) {
				return Observation{}, errors.New("merge commit unknown")
			}
			var commit struct {
				SHA     string `json:"sha"`
				Parents []struct {
					SHA string `json:"sha"`
				} `json:"parents"`
			}
			if err := r.get(ctx, url+"/commits/"+pr.MergeCommit, r.GitHubToken, &commit); err != nil {
				return Observation{}, err
			}
			if commit.SHA != pr.MergeCommit || len(commit.Parents) == 0 {
				return Observation{}, errors.New("merge ancestry unknown")
			}
			// The live base branch can advance after merge. Verify the actual
			// landed commit's first parent instead of assuming current base==base.
			s.Base = commit.Parents[0].SHA
		}
		return s, nil
	case "call":
		if o.Provider != "retell" || o.ProviderOperationID == "" {
			return Observation{}, errors.New("no verified provider lookup identity; auto-redial forbidden")
		}
		var call struct {
			ID     string `json:"call_id"`
			Status string `json:"call_status"`
		}
		if err := r.get(ctx, "https://api.retellai.com/v2/get-call/"+o.ProviderOperationID, r.RetellToken, &call); err != nil {
			return Observation{}, err
		}
		if call.ID != o.ProviderOperationID {
			return Observation{}, errors.New("provider identity mismatch")
		}
		s := Observation{Known: true, ObservedAt: time.Now().UTC(), ProviderOperationID: call.ID}
		switch call.Status {
		case "registered":
			s.ProviderStatus = "queued"
		case "ongoing":
			s.ProviderStatus = "in_progress"
		case "ended":
			s.ProviderStatus = "completed"
		case "error":
			s.ProviderStatus = "failed"
		default:
			s.Known = false
		}
		return s, nil
	default:
		return Observation{}, errors.New("no verified API adapter; operator snapshot required")
	}
}

// SnapshotObserver is for a verified operator read of deployment refs/receipts
// or an SMS provider without a supported adapter. It is not a webhook payload.
type SnapshotObserver struct{ Snapshot Observation }

func (s SnapshotObserver) Observe(context.Context, Operation) (Observation, error) {
	return s.Snapshot, nil
}
