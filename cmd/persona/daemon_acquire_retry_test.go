package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"persona/internal/model"
)

func TestDaemonAcquireLostResponseRetainsClientReleaseIdentity(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "release_without_response"
		if retry {
			name = "retry_without_second_mutation"
		}
		t.Run(name, func(t *testing.T) {
			repo, git := daemonTestRepo(t)
			state := newTestDaemonState(t, repo, git, func() model.GitOps { return &safetyExportGit{} })
			if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
				t.Fatal(err)
			}
			entered, proceed := make(chan struct{}), make(chan struct{})
			state.deps.writeMeta = func(path string, meta daemonSessionMeta) error {
				if meta.BusyOwnerPID > 0 {
					close(entered)
					<-proceed
				}
				return writeDaemonSessionMeta(path, meta)
			}
			// The client saves this identity before sending the request. A lost
			// response must not make the committed acquisition undiscoverable.
			lease := strings.Repeat("ab", 16)
			req := daemonRequest{Method: "acquire_exec", SessionKey: "key", OwnerPID: os.Getpid(), Config: daemonTestConfig(), LeaseID: lease}
			server, client := net.Pipe()
			done := make(chan struct{})
			go func() { state.serveConn(server); close(done) }()
			if err := json.NewEncoder(client).Encode(req); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("acquisition did not reach metadata save")
			}
			_ = client.Close()
			close(proceed)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("disconnected acquisition did not finish")
			}
			state.deps.writeMeta = nil
			if retry {
				resp := daemonServeConnRequest(t, state, req)
				if resp.Error != "" || resp.LeaseID != lease || resp.Session == nil || resp.Session.Generation != 1 {
					t.Fatalf("idempotent retry changed acquisition: %#v", resp)
				}
				other := req
				other.LeaseID = strings.Repeat("cd", 16)
				if resp := daemonServeConnRequest(t, state, other); resp.Error == "" {
					t.Fatal("another operation acquired the outstanding lease")
				}
				other = req
				other.OwnerPID = os.Getppid()
				if resp := daemonServeConnRequest(t, state, other); resp.Error == "" {
					t.Fatal("another owner reused the acquisition")
				}
			}
			resp := daemonServeConnRequest(t, state, daemonRequest{Method: "release_exec", SessionKey: "key", LeaseID: lease})
			if resp.Error != "" {
				t.Fatalf("client could not release without receiving acquire response: %s", resp.Error)
			}
			info, err := state.ensureSession(context.Background(), "key", daemonTestConfig())
			if err != nil || info.Busy || info.Dirty || info.Generation != info.LastDurableGeneration {
				t.Fatalf("canceled acquisition left unsaved/busy state: %#v %v", info, err)
			}
		})
	}
}

func TestDaemonAcquireRejectsInvalidClientIdentity(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &safetyExportGit{} })
	for _, token := range []string{"short", strings.Repeat("A", 32), strings.Repeat("z", 32), strings.Repeat("a", 34)} {
		_, _, err := state.acquireExecWithLease(context.Background(), "key", os.Getpid(), daemonTestConfig(), token)
		if err == nil || state.sessionByKey("key") != nil {
			t.Fatalf("invalid token created a session: %q %v", token, err)
		}
	}
}
