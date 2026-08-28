package doctor

// The upgrade health gate (issue #139, ADR 0044): after `jarvix upgrade`
// installs a fresh build and restarts the daemon, the freshly installed CLI
// runs this critical subset — via `jarvix doctor --gate` — to decide whether
// the new pair may keep serving or must be rolled back.
//
// The subset is the doctor's own check functions, not copies: the ggml
// backend split of 2026-08-25 taught that "installed" and "functional"
// diverge, and the probes that learned that lesson (#113/#114) are exactly
// what an upgrade must re-prove. Duplicating them here would let the two
// drift until the gate waved through a break the doctor could see.

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// GateChecks runs the checks that decide whether an upgrade survives: the
// daemon answers on its socket, it speaks this binary's protocol, and the
// voice loop actually works — whisper really transcribes, the TTS engine
// really synthesizes. Fast environment checks (PipeWire, keybindings,
// provider reachability) stay with the full doctor: a network blip must not
// roll back a good build.
func GateChecks(cfg config.Config, paths config.Paths) []Result {
	checks := []func(config.Config, config.Paths) Result{
		checkDaemon,
		checkProtocol,
		checkSTTProbe,
		checkTTSProbe,
	}
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		results = append(results, check(cfg, paths))
	}
	return results
}

// checkProtocol verifies the daemon speaks the wire protocol this binary was
// compiled against. The pair is installed together, so a mismatch means a
// torn install — exactly the state an interrupted upgrade leaves behind.
func checkProtocol(_ config.Config, paths config.Paths) Result {
	const name = "protocol match"
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return Result{Status: Fail, Name: name,
			Detail: "socket not reachable at " + paths.Socket,
			Fix:    "Start it: systemctl --user start jarvixd"}
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		return Result{Status: Fail, Name: name,
			Detail: "socket reachable but status.get failed: " + err.Error(),
			Fix:    "Restart it: systemctl --user restart jarvixd"}
	}
	if got := jsonNumber(status["protocol"]); int(got) != ipc.ProtocolVersion {
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("the daemon speaks protocol %.0f, this CLI speaks %d — the installed pair is mismatched",
				got, ipc.ProtocolVersion),
			Fix: "Reinstall both binaries from one build: jarvix upgrade (or make install)"}
	}
	return Result{Status: OK, Name: name,
		Detail: fmt.Sprintf("CLI and daemon both speak protocol %d", ipc.ProtocolVersion)}
}
