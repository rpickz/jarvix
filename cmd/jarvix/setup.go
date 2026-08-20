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
