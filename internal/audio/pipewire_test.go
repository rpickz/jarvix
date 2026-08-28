package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PipeWire is never required by these tests: pw-record and pw-play are stub
// shell scripts injected onto PATH. They record their argv, produce/consume
// data like the real tools, and honour SIGINT the way pw-record does.

// pwRecordStub produces a WAV-sized file at the last argument, publishes its
// argv, then waits for SIGINT/SIGTERM like the real recorder. exec makes the
// sleeper *be* the recorded process: the recorder's SIGINT/kill reaches it
// directly, so no child outlives the test (a backgrounded sleep would be
// orphaned — the shell dies, the sleeper lingers for a minute).
//
// Two rules make the stub race-free under arbitrary load (#69):
//
//   - Every published file is written to a .tmp sibling and renamed into
//     place. A shell redirect creates its target empty *before* the writer
//     runs, so a polling test could observe a partial file — an empty argv
//     capture, or a wav still short of Stop's 1024-byte empty-recording
//     floor. rename(2) is atomic: once the final name exists, the content
//     is complete.
//   - The argv capture is published last. Tests gate on it before stopping
//     or cancelling, so its existence proves the stub has finished every
//     write — an interrupt can then only land on the sleeper, never orphan
//     a mid-write child to race the test's TempDir cleanup.
const pwRecordStub = `#!/bin/sh
for last in "$@"; do :; done
head -c "${JARVIX_STUB_WAV_BYTES:-4096}" /dev/zero > "$last.tmp"
mv "$last.tmp" "$last"
printf '%s\n' "$@" > "$JARVIX_STUB_DIR/pw-record.args.tmp"
mv "$JARVIX_STUB_DIR/pw-record.args.tmp" "$JARVIX_STUB_DIR/pw-record.args"
exec sleep 60
`

// pwPlayStub publishes its argv (by rename, for the same reason as
// pwRecordStub's), consumes stdin to a file, then exits with the scripted
// status. Unless a failure status is scripted, the shell execs cat so the
// stdin consumer *is* the recorded process: cancellation's kill then cannot
// leave an orphaned cat behind to create the capture file while t.TempDir
// cleanup is deleting the directory (seen under load as "TempDir RemoveAll
// cleanup: … directory not empty"). The stdin capture itself needs no
// rename: tests only read it after Play returns, i.e. after the stub exited.
const pwPlayStub = `#!/bin/sh
printf '%s\n' "$@" > "$JARVIX_STUB_DIR/pw-play.args.tmp"
mv "$JARVIX_STUB_DIR/pw-play.args.tmp" "$JARVIX_STUB_DIR/pw-play.args"
if [ "${JARVIX_STUB_EXIT:-0}" = 0 ]; then
	exec cat > "$JARVIX_STUB_DIR/pw-play.stdin"
fi
cat > "$JARVIX_STUB_DIR/pw-play.stdin"
exit "$JARVIX_STUB_EXIT"
`

// installStubs puts fake pw-record/pw-play first on PATH and returns the
// directory where the stubs drop their argv/stdin captures.
func installStubs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{"pw-record": pwRecordStub, "pw-play": pwPlayStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JARVIX_STUB_DIR", dir)
	return dir
}

// waitForFile blocks until the stub has produced path. Stubs publish files by
// atomic rename, so existence implies the content is complete; this poll is
// therefore an event wait on the stub's readiness, not a race against its
// writes. The deadline only bounds a genuinely broken stub.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stub never produced %s", path)
}

func stubArgs(t *testing.T, stubDir, name string) []string {
	t.Helper()
	waitForFile(t, filepath.Join(stubDir, name+".args"))
	data, err := os.ReadFile(filepath.Join(stubDir, name+".args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestPipeWireRecorderBuildsCaptureArgv(t *testing.T) {
	stubDir := installStubs(t)
	r := &PipeWireRecorder{Dir: t.TempDir(), Device: "my-mic", MaxDuration: time.Minute}

	rec, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args := stubArgs(t, stubDir, "pw-record")
	// whisper.cpp needs 16 kHz mono s16; the WAV path must come last.
	want := []string{"--rate", "16000", "--channels", "1", "--format", "s16", "--target", "my-mic"}
	if len(args) != len(want)+1 {
		t.Fatalf("argv = %q", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv = %q, want prefix %q", args, want)
		}
	}
	wavPath := args[len(args)-1]
	if filepath.Dir(wavPath) != r.Dir || !strings.HasSuffix(wavPath, ".wav") {
		t.Errorf("wav path = %q, want a .wav in %q", wavPath, r.Dir)
	}
	waitForFile(t, wavPath)

	clip, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if clip.WAVPath != wavPath || clip.SampleRate != 16000 || clip.Channels != 1 {
		t.Errorf("clip = %+v", clip)
	}
	if _, err := os.Stat(clip.WAVPath); err != nil {
		t.Errorf("clip file missing: %v", err)
	}
}

func TestPipeWireRecorderOmitsTargetWithoutDevice(t *testing.T) {
	stubDir := installStubs(t)
	r := &PipeWireRecorder{Dir: t.TempDir()}
	rec, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Cancel()
	args := stubArgs(t, stubDir, "pw-record")
	for _, a := range args {
		if a == "--target" {
			t.Errorf("argv %q must not include --target for the default source", args)
		}
	}
}

func TestPipeWireRecorderRejectsEmptyRecording(t *testing.T) {
	stubDir := installStubs(t)
	// A file barely beyond the WAV header means nothing was captured.
	t.Setenv("JARVIX_STUB_WAV_BYTES", "100")
	r := &PipeWireRecorder{Dir: t.TempDir()}
	rec, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args := stubArgs(t, stubDir, "pw-record")
	waitForFile(t, args[len(args)-1]) // let the stub finish writing the capture
	_, err = rec.Stop()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Stop on an empty capture = %v, want empty-recording error", err)
	}
}

func TestPipeWireRecorderCancelDiscardsFile(t *testing.T) {
	stubDir := installStubs(t)
	r := &PipeWireRecorder{Dir: t.TempDir()}
	rec, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args := stubArgs(t, stubDir, "pw-record")
	wavPath := args[len(args)-1]
	waitForFile(t, wavPath)

	rec.Cancel()
	if _, err := os.Stat(wavPath); !os.IsNotExist(err) {
		t.Errorf("cancelled recording left %s behind", wavPath)
	}
	// Stop after Cancel must not resurrect the clip.
	if _, err := rec.Stop(); err == nil {
		t.Error("Stop after Cancel should report no file")
	}
}

func TestPipeWireRecorderStartFailsWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no pw-record anywhere
	r := &PipeWireRecorder{Dir: t.TempDir()}
	_, err := r.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pw-record") {
		t.Fatalf("err = %v, want a pw-record start failure", err)
	}
}

func TestPipeWirePlayerBuildsPlaybackArgvAndStreamsPCM(t *testing.T) {
	stubDir := installStubs(t)
	p := &PipeWirePlayer{Device: "my-speaker"}

	chunks := make(chan []byte, 3)
	chunks <- []byte("abc")
	chunks <- []byte("def")
	chunks <- []byte("g")
	close(chunks)
	if err := p.Play(context.Background(), 22050, 1, chunks); err != nil {
		t.Fatal(err)
	}

	want := []string{"--rate", "22050", "--channels", "1", "--format", "s16", "--raw", "--target", "my-speaker", "-"}
	args := stubArgs(t, stubDir, "pw-play")
	if len(args) != len(want) {
		t.Fatalf("argv = %q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv = %q, want %q", args, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(stubDir, "pw-play.stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abcdefg" {
		t.Errorf("pw-play received %q, want %q", data, "abcdefg")
	}
}

func TestPipeWirePlayerOmitsTargetWithoutDevice(t *testing.T) {
	stubDir := installStubs(t)
	p := &PipeWirePlayer{}
	chunks := make(chan []byte, 1)
	chunks <- []byte("pcm")
	close(chunks)
	if err := p.Play(context.Background(), 24000, 1, chunks); err != nil {
		t.Fatal(err)
	}
	// No --target: the stream follows the default sink, which is what lets
	// WirePlumber move live speech to a newly-chosen device (issue #142).
	// This is the chosen binding behaviour, pinned.
	for _, a := range stubArgs(t, stubDir, "pw-play") {
		if a == "--target" {
			t.Fatalf("argv %q must not include --target for the default sink", stubArgs(t, stubDir, "pw-play"))
		}
	}
}

// pwPlayDyingStub scripts the mid-stream death of issue #142's diagnosis. Its
// first invocation consumes exactly JARVIX_STUB_DIE_AFTER bytes (dd bs=1
// reads byte-at-a-time, so it can never over-consume from the pipe), closes
// its stdin read end, publishes a marker by atomic rename, and exits — so
// once the marker exists, the next write into the pipe deterministically
// fails. Every later invocation consumes everything, like a healthy pw-play.
// Captures are indexed per invocation; invocations are sequential (Play
// respawns serially), so the count file needs no locking.
const pwPlayDyingStub = `#!/bin/sh
n=$(cat "$JARVIX_STUB_DIR/pw-play.count" 2>/dev/null || echo 0)
echo $((n+1)) > "$JARVIX_STUB_DIR/pw-play.count"
printf '%s\n' "$@" > "$JARVIX_STUB_DIR/pw-play.args.tmp.$n"
mv "$JARVIX_STUB_DIR/pw-play.args.tmp.$n" "$JARVIX_STUB_DIR/pw-play.args.$n"
if [ "$n" = 0 ] && [ -n "${JARVIX_STUB_DIE_AFTER:-}" ]; then
	dd bs=1 count="$JARVIX_STUB_DIE_AFTER" of="$JARVIX_STUB_DIR/pw-play.stdin.$n" status=none
	exec 0<&-
	printf done > "$JARVIX_STUB_DIR/pw-play.died.tmp"
	mv "$JARVIX_STUB_DIR/pw-play.died.tmp" "$JARVIX_STUB_DIR/pw-play.died"
	exit 9
fi
exec cat > "$JARVIX_STUB_DIR/pw-play.stdin.$n"
`

func installDyingPlayStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pw-play"), []byte(pwPlayDyingStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JARVIX_STUB_DIR", dir)
	return dir
}

// TestPipeWirePlayerResumesAfterMidStreamDeath pins the issue #142 fix: a
// pw-play that dies with audio still to play is replaced, and the replacement
// receives exactly the bytes the dead process never accepted — no byte lost
// from the stream Jarvix still holds, no byte written twice.
func TestPipeWirePlayerResumesAfterMidStreamDeath(t *testing.T) {
	stubDir := installDyingPlayStub(t)
	t.Setenv("JARVIX_STUB_DIE_AFTER", "6")
	p := &PipeWirePlayer{}

	chunks := make(chan []byte)
	done := make(chan error, 1)
	go func() { done <- p.Play(context.Background(), 24000, 1, chunks) }()

	// Feed the dying process its quota, then wait for it to be dead — the
	// marker is published only after its stdin is closed, so the next write
	// must fail rather than land in a live pipe buffer.
	chunks <- []byte("aaaa")
	chunks <- []byte("bb")
	waitForFile(t, filepath.Join(stubDir, "pw-play.died"))
	chunks <- []byte("cccc")
	close(chunks)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Play = %v, want playback resumed on a fresh process", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return")
	}
	first, err := os.ReadFile(filepath.Join(stubDir, "pw-play.stdin.0"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "aaaabb" {
		t.Errorf("first pw-play consumed %q, want %q", first, "aaaabb")
	}
	second, err := os.ReadFile(filepath.Join(stubDir, "pw-play.stdin.1"))
	if err != nil {
		t.Fatalf("no second pw-play was spawned: %v", err)
	}
	if string(second) != "cccc" {
		t.Errorf("resumed pw-play received %q, want %q", second, "cccc")
	}
}

// TestPipeWirePlayerDrainsProducerOnTerminalFailure pins the Player contract
// the issue #142 diagnosis found broken: when playback cannot happen at all,
// Play must still consume the channel until it closes, because its producers
// (the speaker) block on the handoff and only read the result afterwards. An
// early return without draining wedged the rest of the answer for the life of
// the session.
func TestPipeWirePlayerDrainsProducerOnTerminalFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no pw-play anywhere: playback is impossible
	p := &PipeWirePlayer{}

	chunks := make(chan []byte)
	done := make(chan error, 1)
	go func() { done <- p.Play(context.Background(), 24000, 1, chunks) }()

	// The producer keeps feeding after the failure, exactly as a synthesizer
	// mid-answer does; every handoff must be accepted.
	for i := 0; i < 32; i++ {
		select {
		case chunks <- []byte("pcm"):
		case <-time.After(5 * time.Second):
			t.Fatal("producer blocked feeding a failed playback")
		}
	}
	close(chunks)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "pw-play") {
			t.Fatalf("err = %v, want a pw-play failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return after the channel closed")
	}
}

func TestPipeWirePlayerReportsProcessFailure(t *testing.T) {
	installStubs(t)
	t.Setenv("JARVIX_STUB_EXIT", "3")
	p := &PipeWirePlayer{}
	chunks := make(chan []byte)
	close(chunks)
	err := p.Play(context.Background(), 22050, 1, chunks)
	if err == nil || !strings.Contains(err.Error(), "pw-play failed") {
		t.Fatalf("err = %v, want pw-play failure", err)
	}
}

func TestPipeWirePlayerCancellationStopsPlayback(t *testing.T) {
	stubDir := installStubs(t)
	p := &PipeWirePlayer{}
	ctx, cancel := context.WithCancel(context.Background())

	chunks := make(chan []byte, 1)
	chunks <- []byte("audio")
	// The channel stays open: playback is mid-stream when the user interrupts.
	done := make(chan error, 1)
	go func() { done <- p.Play(ctx, 22050, 1, chunks) }()
	stubArgs(t, stubDir, "pw-play") // pw-play is up and consuming
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return after cancellation")
	}
}

func TestPipeWirePlayerStartFailsWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p := &PipeWirePlayer{}
	chunks := make(chan []byte)
	close(chunks)
	err := p.Play(context.Background(), 22050, 1, chunks)
	if err == nil || !strings.Contains(err.Error(), "pw-play") {
		t.Fatalf("err = %v, want a pw-play start failure", err)
	}
}
