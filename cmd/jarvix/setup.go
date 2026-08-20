package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
)

// udevRule grants the active seat's logged-in user read access to keyboard
// event devices via an ACL (TAG+="uaccess"), scoped to keyboards only. This
// is what lets jarvixd see the push-to-talk chord's real press and release.
const udevRule = `# Jarvix push-to-talk: let the logged-in user read keyboard events.
# Installed by 'jarvix setup input'. Remove this file to revoke.
KERNEL=="event*", SUBSYSTEM=="input", ENV{ID_INPUT_KEYBOARD}=="1", TAG+="uaccess"
`

const udevRulePath = "/etc/udev/rules.d/70-jarvix-input.rules"

// cmdSetupInput installs the udev rule for daemon-side push-to-talk, or
// prints the exact commands when not running as root.
func cmdSetupInput() error {
	fmt.Println("Daemon-side push-to-talk needs read access to keyboard event devices.")
	fmt.Println("Note: this lets any process running as your user read key events —")
	fmt.Println("the same trade-off every push-to-talk app on Linux makes. Jarvix only")
	fmt.Println("tracks its configured chord and never logs keys (see docs/adr/0008).")
	fmt.Println()
	if os.Geteuid() == 0 {
		if err := os.WriteFile(udevRulePath, []byte(udevRule), 0o644); err != nil {
			return err
		}
		fmt.Println("Installed", udevRulePath)
		fmt.Println("Now run: udevadm control --reload && udevadm trigger")
		return nil
	}
	fmt.Println("Run these commands:")
	fmt.Println()
	fmt.Printf("  sudo tee %s <<'EOF'\n%sEOF\n", udevRulePath, udevRule)
	fmt.Println("  sudo udevadm control --reload && sudo udevadm trigger")
	fmt.Println("  systemctl --user restart jarvixd")
	fmt.Println()
	fmt.Println("Then verify with: jarvix doctor")
	return nil
}

// cmdSetupWhisper downloads a Whisper model into the XDG data directory.
func cmdSetupWhisper(paths config.Paths, model string) error {
	url, ok := whispercpp.ModelURL(model)
	if !ok {
		names := make([]string, 0, len(whispercpp.KnownModels))
		for name := range whispercpp.KnownModels {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown model %q; known models: %s", model, strings.Join(names, ", "))
	}
	dest := whispercpp.ResolveModelPath(model, paths.WhisperModelDir())
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("Model %s already present at %s\n", model, dest)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	fmt.Printf("Downloading %s\n  from %s\n  to   %s\n", model, url, dest)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, &progressReader{r: resp.Body, total: resp.ContentLength})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(tmp)
		if err == nil {
			err = closeErr
		}
		return fmt.Errorf("download failed: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	fmt.Printf("\nDone (%d MB)\n", written/1024/1024)
	return nil
}

type progressReader struct {
	r     io.Reader
	total int64
	read  int64
	last  time.Time
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Printf("\r  %d%% (%d/%d MB)", p.read*100/p.total, p.read/1024/1024, p.total/1024/1024)
		} else {
			fmt.Printf("\r  %d MB", p.read/1024/1024)
		}
	}
	return n, err
}
