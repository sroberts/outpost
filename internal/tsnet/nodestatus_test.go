package tsnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A node waiting to be authorised says where, and stops saying so once it
// is on the tailnet.
func TestNodeStatus_LoginURLUntilAttached(t *testing.T) {
	authorised := make(chan struct{})
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		cfg.onLogin("https://login.example/a/1")
		select {
		case <-authorised:
			return newFakeNode(t), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if _, ok := r.NodeStatus("yarr"); ok {
		t.Error("a node is reported for a service never attached")
	}
	if err := r.Attach(context.Background(), svc("yarr", 9301)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "needs-login", func() bool {
		st, _ := r.NodeStatus("yarr")
		return st.State == NodeNeedsLogin
	})
	if st, _ := r.NodeStatus("yarr"); st.LoginURL != "https://login.example/a/1" {
		t.Errorf("status = %+v; want the login URL", st)
	}

	close(authorised)
	waitFor(t, 5*time.Second, "on", func() bool {
		st, _ := r.NodeStatus("yarr")
		return st.State == NodeOn
	})
	if st, _ := r.NodeStatus("yarr"); st.LoginURL != "" || st.Error != "" {
		t.Errorf("status = %+v; want the URL and error gone once attached", st)
	}

	if err := r.Detach(context.Background(), "yarr"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.NodeStatus("yarr"); ok {
		t.Error("a detached node is still reported")
	}
}

// A node that keeps failing is starting, with the reason, rather than on.
func TestNodeStatus_FailingNodeSaysWhy(t *testing.T) {
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		return nil, errors.New("coordination server unreachable")
	})
	if err := r.Attach(context.Background(), svc("yarr", 9302)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the error", func() bool {
		st, _ := r.NodeStatus("yarr")
		return st.Error != ""
	})
	st, _ := r.NodeStatus("yarr")
	if st.State != NodeStarting || st.Error != "coordination server unreachable" || st.LoginURL != "" {
		t.Errorf("status = %+v", st)
	}
}

// TestHelperLoginNodeProcess is not a test. It is a child node that reports
// a login URL the way the real one does, then waits to be stopped.
func TestHelperLoginNodeProcess(t *testing.T) {
	if os.Getenv("OUTPOST_HELPER_LOGIN_NODE") != "1" {
		return
	}
	fmt.Println(LoginLinePrefix + "https://login.tailscale.com/a/xyz789")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// The login line from a real child process reaches the status, and isn't
// logged as if it were node output.
func TestProcessNode_LoginLineReachesStatus(t *testing.T) {
	logs := &logSink{}
	r, err := New(Options{StateDir: t.TempDir(), Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	r.nodeCmd = func(cfg nodeConfig) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLoginNodeProcess$")
		cmd.Env = append(os.Environ(), "OUTPOST_HELPER_LOGIN_NODE=1")
		return cmd, nil
	}
	r.nodeStopGrace = 2 * time.Second
	t.Cleanup(func() { _ = r.Close() })

	if err := r.Attach(context.Background(), svc("newbie", 9303)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the login URL", func() bool {
		st, _ := r.NodeStatus("newbie")
		return st.LoginURL == "https://login.tailscale.com/a/xyz789"
	})
	if st, _ := r.NodeStatus("newbie"); st.State != NodeNeedsLogin {
		t.Errorf("state = %q", st.State)
	}
	if logs.has(LoginLinePrefix) {
		t.Errorf("the protocol line was logged: %q", logs.snapshot())
	}
}

// A node that exits before coming up is reported by the line that says why,
// not the whole tail the log gets.
func TestProcessNode_EarlyExitReportsItsReason(t *testing.T) {
	r, logs, _ := processRouter(t, "fail", "")
	if err := r.Attach(context.Background(), svc("broken", 9304)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the reason", func() bool {
		st, _ := r.NodeStatus("broken")
		return st.Error != ""
	})
	if st, _ := r.NodeStatus("broken"); st.Error != "start tsnet node: state directory is locked" {
		t.Errorf("error = %q", st.Error)
	}
	if !logs.has("exited before coming up: start tsnet node: state directory is locked") {
		t.Errorf("the log lost the detail: %q", logs.snapshot())
	}
}
