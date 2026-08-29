// Package session owns the conversational core of jarvixd: the authoritative
// session state machine, the event bus, and the engine that drives one
// interaction from captured speech through transcription, the assistant
// (including tool rounds), and streamed spoken output.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/quiesce"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// Options tunes engine behaviour. Values come from configuration.
type Options struct {
	Model          string
	SystemPrompt   string
	MaxTokens      int
	Temperature    float64
	SpeakResponses bool
	// MinRecording discards captures shorter than this as accidental
	// activations (a stray key tap) instead of transcribing them.
	MinRecording time.Duration
	// HistoryTurns is how many prior user+assistant exchanges to keep as
	// conversation context. Zero disables memory (each turn is standalone).
	HistoryTurns int
	// FollowUpWindow resets the conversation when this long has passed since
	// the last exchange, so an old thread does not bleed into a new one. Zero
	// keeps history until an explicit reset.
	FollowUpWindow time.Duration
	// ConfirmTimeout is how long a pending tool confirmation waits for the
	// user before declining. Zero means DefaultConfirmTimeout.
	ConfirmTimeout time.Duration
	// ConfirmTimer overrides the confirmation-timeout clock; nil uses a real
	// time.Timer. It exists so daemon-level socket tests can fire a timeout
	// deterministically — the validated config cannot express a timeout
	// shorter than a second, and a test that waited one out would be a sleep
	// wearing a costume. Never set in production.
	ConfirmTimer func(d time.Duration) (<-chan time.Time, func())
	// SpeakConfirmationDetails reads the full generated confirmation question
	// aloud — the one that quotes the command — instead of the default short
	// prompt that names the kind of action and points at the screen
	// (confirmations.speak_details, issue #119). Audio only: the events and
	// the record always carry the full summary and the verbatim command, so
	// the visual surfaces are unaffected (ADR 0014).
	SpeakConfirmationDetails bool
	// RememberApprovals re-runs a user-approved command without asking again
	// for the rest of the conversation. Approvals live in memory only and
	// are cleared with the conversation — they never survive `jarvix new`,
	// the follow-up window, or a daemon restart.
	RememberApprovals bool
	// Intents is the deterministic intent router consulted before every
	// provider call (ADR 0017). Nil disables routing: every transcript goes
	// to the model, exactly as it did before the router existed.
	Intents *intent.Router
	// IntentRunner executes matched intents. Nil alongside a router installs
	// the real one; tests substitute a fake so no test touches wpctl.
	IntentRunner intent.Runner
	// Compositor carries out the intents that act on the desktop — "workspace
	// four", "open a terminal" — through the same seam the window tools use
	// (ADR 0022), so the dispatch dialect is probed once for both.
	//
	// Deliberately not defaulted the way IntentRunner is: a nil here means
	// those intents say they cannot reach the window manager, which is both
	// the honest answer off a Wayland session and the reason no test in this
	// package can accidentally move the developer's workspace.
	Compositor desktop.Compositor
	// Routines executes the named routines the intent router matches
	// (ADR 0026). Nil — a daemon with no [[routines]] configured — makes a
	// matched routine phrase an honest spoken refusal, though validated
	// configuration cannot produce that combination: the router only knows
	// phrases the same config that builds the runner declared.
	Routines RoutineRunner
	// Scripts executes the named scripts the intent router matches
	// (ADR 0030). Nil — a daemon with no [[scripts]] configured — makes a
	// matched script phrase an honest spoken refusal, though validated
	// configuration cannot produce that combination: the router only knows
	// phrases the same config that builds the runner declared.
	Scripts ScriptRunner
	// Capture plans and writes "save this as <name>" layout captures (#62).
	// Nil — a daemon built without the capture service — makes a matched
	// capture phrase an honest spoken refusal rather than a silent drop.
	Capture RoutineCapturer
	// WindowNames assigns and lists window nicknames (#126) through the
	// window tools' shared resolution seam. Nil — a daemon whose window
	// tools are switched off — makes a matched nickname phrase an honest
	// spoken refusal rather than a silent drop.
	WindowNames WindowNamer
	// MonitorNames assigns, forgets and lists monitor nicknames (#180)
	// through the same shared seam. Nil — a daemon whose window tools are
	// switched off — makes a matched screen-name phrase an honest spoken
	// refusal rather than a silent drop.
	MonitorNames MonitorNamer
	// Context gathers opt-in desktop context — active window, selection,
	// clipboard — for turns that reach the model (ADR 0019). Nil disables it
	// entirely: no gathering, no message, no cost.
	Context ContextCollector
	// Memory supplies the remembered-facts block — the user-curated knowledge
	// base (ADR 0025) — for turns that reach the model. Nil disables it
	// entirely: no consultation, no message, no cost.
	Memory MemoryInjector
	// Vocabulary supplies the taught-words block — the phrases the user
	// explicitly taught (issue #129) — for turns that reach the model. Nil
	// disables it entirely: no consultation, no message, no cost.
	Vocabulary VocabularyInjector
	// VocabularyTeacher stores what the deterministic teach phrases match —
	// "when i say X i mean Y", "listen for the word X", the spoken listing
	// (issue #129). Nil — vocabulary disabled — makes a matched teach phrase
	// an honest spoken refusal rather than a silent drop.
	VocabularyTeacher VocabularyTeacher
	// Approvals answers "what have i pre-approved" (issue #162). Nil makes
	// the matched phrase an honest spoken refusal rather than a confident
	// "nothing" — a listing that says the grants are empty when it simply
	// cannot see them is the worst possible answer to a question about
	// permissions.
	Approvals ApprovalsLister
	// Provenance answers "where did that come from" (issue #168) from the
	// record the engine already holds. Nil makes the matched phrase an honest
	// spoken refusal rather than a confident "nothing" — the same asymmetry
	// the approvals listing draws, and for the same reason: a listing that
	// reports no sources because it cannot see them would be the one answer
	// this feature must never give.
	Provenance ProvenanceLister
	// Knowledge supplies the live feed values block — cached readings from
	// the user's configured feeds (ADR 0031) — for turns that reach the
	// model. Nil disables it entirely: no consultation, no message, no cost.
	Knowledge KnowledgeInjector
	// Archive is the durable conversation store (ADR 0027). Every completed
	// exchange is appended to it *before* HistoryTurns trims the in-memory
	// head: the cap governs what the model is sent, never what is kept. Nil
	// disables archiving entirely (conversation.retention = "off"): nothing
	// staged, nothing written, `jarvix new` behaves as before the archive
	// existed.
	Archive conversations.Store
	// WakeWord is the assistant's configured name ([assistant] name, issue
	// #103) — the summons background listening answers to (ADR 0024). It is
	// here because the name is *in* the transcript: the pre-roll
	// deliberately includes it, so whisper returns "Jarvix, volume thirty".
	// Left in place, that utterance would never match the intent router,
	// which matches strictly against the whole thing. It may be multi-word
	// ("Mister Smith"); empty (the default) strips nothing.
	WakeWord string
	// WakeAliases are additional spellings the strip accepts as the name —
	// whisper's known mishearings of it ([assistant] aliases; issue #83).
	// They widen only what stripWakeWord removes from a wake transcript; the
	// acoustic wake gate never sees them. Empty accepts the name alone.
	WakeAliases []string
	// Lexicon respells terms the voice mispronounces, term → spoken form
	// ([tts.lexicon]). Merged over the shipped defaults; nil is the defaults
	// alone. Spoken output only — the overlay shows the original text.
	Lexicon map[string]string
	// Returning is the return briefing's seam (#150, ADR 0050). Nil — a
	// daemon built without the briefing service — makes every hook a no-op
	// and a matched briefing phrase an honest spoken refusal, the same
	// disabled-means-absent rule the other collaborators follow.
	Returning Returning
}

// Engine owns the session lifecycle: one active session at a time, one
// authoritative state, cancellation from every active state, and interruption
// (a new session while speaking) as a first-class operation.
type Engine struct {
	provider ai.Provider
	stt      stt.Transcriber
	tts      tts.Synthesizer
	recorder audio.Recorder
	player   audio.Player
	tools    *tools.Registry
	bus      *Bus
	log      *slog.Logger
	opts     Options

	// store persists conversation memory across daemon restarts (ADR 0011);
	// nil disables persistence. Immutable after construction, so it is read
	// without the lock.
	store history.Store
	// persistFailed latches after the first failed save: the engine degrades
	// to in-memory-only for its lifetime and warns exactly once, instead of
	// spamming a warning per exchange on a persistently broken disk.
	persistFailed atomic.Bool
	// now is the follow-up-window clock, injectable so tests can lapse the
	// window deterministically — including across a simulated restart.
	now func() time.Time
	// timer is the confirmation-timeout clock, injectable so tests can fire
	// (or withhold) the timeout deterministically. The returned stop func
	// releases the underlying timer.
	timer func(d time.Duration) (<-chan time.Time, func())
	// progressAfter is how long a slow tool call may run before Jarvix says
	// it is still working (ADR 0016). Immutable after construction; tests
	// shorten it.
	progressAfter time.Duration
	// speakerQueued, when non-nil, runs on the enqueueing goroutine the
	// instant an utterance has been handed to the speaker's run loop. It is
	// the seam for issue #80's deterministic interleaving test: a slow CI
	// runner parked the enqueuer exactly there while the answer went on to
	// announce itself, and the test parks it on purpose to pin that window.
	// Nil in production; set only before a session starts.
	speakerQueued func()
	// speech renders assistant text as its spoken form, carrying the
	// configured pronunciation lexicon. Rebuilt (never mutated) by
	// Reconfigure, so a session already speaking keeps the one it started
	// with; read without the lock like the other swappable collaborators.
	speech *speechNormalizer

	// active tracks the session goroutines (transcribe, think, runIntent,
	// recording teardown) that read the swappable collaborators and options
	// without holding mu. Reconfigure waits on it so a swap never races a draining
	// goroutine — a cancelled session's think() can still be executing briefly
	// after current is nil (ADR 0015) — and Shutdown waits on it so the daemon
	// does not exit through the tail of a finished session, which is where the
	// post-finish history write lives (ADR 0011).
	active quiesce.Group

	mu      sync.Mutex
	state   State
	current *sess
	counter int
	// stateSince is when the current state began, on the engine's own clock.
	// It exists so a surface can say how long a wait has been going without
	// guessing (issue #158): the bar's counter starts when the widget *saw*
	// the transition, which is right for a widget that was already running and
	// wrong for a window opened five seconds into a long think — it would
	// start from zero and tell the user a comfortable lie. Published as
	// state.changed's since_ms and conversation.get's state_since_ms; guarded
	// by mu.
	stateSince time.Time
	// runningTool is the tool call executing right now, with the progress
	// label it published on tool.started when it has one. A window that opens
	// mid-tool-round reconstructs the pending turn's wording from these
	// (conversation.get), so it says "Consulting claude · 12s" exactly like a
	// window that watched the round start. Cleared when the call returns and
	// again when the session ends, so a hung tool cannot outlive its turn on
	// screen. Guarded by mu.
	runningTool       string
	runningToolDetail string
	// pending is the tool confirmation the session is waiting on, if any
	// (ADR 0014). Guarded by mu.
	pending *pendingConfirmation
	// approvals are commands the user already confirmed this conversation
	// (remember_for_conversation). Cleared with the conversation; guarded
	// by mu.
	approvals map[string]bool
	// grants are conversation-scoped allow patterns (issue #162, ADR 0053):
	// word prefixes the user granted on a confirmation card by choosing
	// "just this conversation". They are the same vocabulary as
	// `[tools.policy] shell_allow` and are applied by the same classifier,
	// but they exist HERE and nowhere else — never written, never read back,
	// gone when the process ends or the conversation does.
	//
	// That is the whole promise of the scope, and it is kept structurally by
	// where the field lives rather than by remembering not to persist it:
	// there is no writer on this path to forget to disable. Cleared beside
	// approvals everywhere approvals are cleared, because a grant is an
	// approval with a broader shape and must not outlive one.
	//
	// Ordered rather than a set: the classifier walks patterns in order and
	// names the first that matched in the audit row, so the row a user sees
	// is stable across runs. Guarded by mu.
	grants []string
	// reconfiguring blocks new sessions for the brief window in which
	// Reconfigure drains e.active before swapping collaborators.
	reconfiguring bool
	// shuttingDown latches in Shutdown: the engine is stopping for good and
	// refuses new sessions from that moment on, so the drain has a door it can
	// close rather than chasing work that keeps arriving.
	shuttingDown bool

	// Conversation memory: prior exchanges carried across sessions so
	// follow-up questions have context. Guarded by mu.
	history  []ai.Message
	lastTurn time.Time
	// msgCount counts every message ever committed to the live thread —
	// including any the retention cap has trimmed, and the pairs counted
	// while in-memory history is disabled — so confirmation records can be
	// anchored to positions that never shift under the cap (issue #118;
	// confirmrecord.go). Guarded by mu; reset with the thread.
	msgCount int
	// confRecords are the resolved permission-gate exchanges of the live
	// thread, in resolution order (afterMsgs nondecreasing), rendered by
	// Conversation() between the turns they belong to. They are display and
	// archive state, never model context: the model already hears about each
	// outcome through the tool round itself. Guarded by mu.
	confRecords []*confirmationRecord
	// provRecords is what went into each answer of the live thread (issue
	// #168), anchored to the assistant message it describes on the same
	// counter and for the same reason as confRecords. Display and archive
	// state, never model context — the model is not told what it was given
	// twice. Guarded by mu. See provenance.go.
	provRecords []provRecord

	// The durable archive's view of the thread (ADR 0027; archive.go).
	// archiveID is which archived conversation the live thread belongs to
	// ("" until the first flush names one); pendingArchive holds completed
	// exchanges staged by commitTurn and not yet flushed; archiveGen counts
	// thread changes (reset, reopen, lapse) so an in-flight flush can tell
	// its thread has ended and must not adopt a conversation id for the next
	// one. All three guarded by mu. archiveMu serialises whole flushes (see
	// persistArchive); archiveFailed latches like persistFailed — one warning,
	// then in-memory-only for the engine's lifetime.
	archiveID      string
	pendingArchive []conversations.Turn
	archiveGen     int
	archiveMu      sync.Mutex
	archiveFailed  atomic.Bool

	// The most recent desktop context capture, kept so the user can always
	// audit what Jarvix saw (ADR 0019). It outlives its session deliberately —
	// `jarvix status --last` is asked *after* the answer — but never outlives
	// the daemon: context is not persisted. Guarded by mu.
	lastContext        desktop.Snapshot
	lastContextSession string
	lastContextTaken   bool

	// The most recent knowledge-base injection, kept for the same audit
	// promise memory.last / `jarvix status --last` make for desktop context
	// (ADR 0025). Guarded by mu.
	lastMemory        memory.Injection
	lastMemorySession string
	lastMemoryTaken   bool

	// The most recent vocabulary injection (issue #129), on memory's exact
	// audit terms: what taught words the model was given must be answerable
	// after the fact (vocabulary.last). Guarded by mu.
	lastVocabulary        vocabulary.Injection
	lastVocabularySession string
	lastVocabularyTaken   bool
}

// sess is one interaction from start to finish.
type sess struct {
	id           string
	ctx          context.Context
	cancel       context.CancelFunc
	recording    audio.Recording
	started      time.Time
	voiceStarted time.Time
	// timings carries the per-stage latency marks of this interaction; it is
	// written from every stage goroutine and published when the session ends.
	timings timings

	// A session proceeds to Thinking only once both are true: the transcript
	// is ready (from STT or provided directly) and the client has submitted.
	transcript      string
	transcriptReady bool
	submitted       bool
	// transcriptReason explains a transcript that arrived deliberately empty:
	// the capture held no voiced audio, or whisper handed the bias prompt
	// back instead of speech (issue #191). It is what the turn reports
	// instead of inventing one, and it is the sentence a user debugging a
	// microphone reads in the activity feed. Guarded by Engine.mu.
	transcriptReason string

	// userText is the transcript the turn actually processed — wake word
	// stripped, snapshotted in maybeThinkLocked the moment the turn leaves
	// the input stage. It exists because s.transcript is not stable for the
	// turn's whole life: a voice reply to a tool confirmation overwrites it
	// ("yes" replacing the question), and anything committed to the record
	// afterwards must remember the question, not the reply. Guarded by
	// Engine.mu; written exactly once.
	userText string
	// committed marks the exchange as recorded — by the normal tail
	// (commitTurn) or by an interrupted commit (issue #117), whichever ran
	// first. It closes the race between a cancel and a turn that had already
	// reached its commit: without it, a cancel landing between commitTurn and
	// finishLocked would record the exchange twice, once whole and once
	// marked interrupted. Guarded by Engine.mu.
	committed bool

	// streamed accumulates the assistant text of every round as it streams,
	// so an interruption can commit what the user actually heard begun
	// (issue #117): the incident's clarifying question was fully streamed and
	// still lost, because the only copy lived on think()'s stack. Its own
	// mutex rather than Engine.mu, because appends happen per delta on the
	// streaming hot path and reads happen from cancel paths that already hold
	// Engine.mu — the ordering is always mu → streamedMu, never the reverse.
	streamedMu sync.Mutex
	streamed   strings.Builder

	// prov accumulates what went into this turn (issue #168): the references
	// noted by each injection as it is gathered, and by each tool call as it
	// returns. Collected here rather than reconstructed later because here is
	// the one point every one of them is already known.
	//
	// Its own mutex, on the streamed builder's terms and for the same reason:
	// appends happen on the turn's goroutine while reads happen from commit
	// paths that already hold Engine.mu, so the ordering is always
	// mu → provMu, never the reverse.
	provMu sync.Mutex
	prov   []provenance.Reference

	// replyCapture marks a voice capture that answers a pending tool
	// confirmation rather than asking a new question: its transcript is
	// interpreted as yes/no and resolves the confirmation instead of
	// starting a think round.
	replyCapture bool

	// wake marks a session a wake word started (ADR 0024). Its transcript
	// begins with the wake word, which is stripped before anything reads it.
	wake bool

	// quiet marks a session whose turn must not be spoken aloud — a
	// schedule's clockfire with announce off (ADR 0032). Every speech exit
	// (the streaming speaker, the intent acknowledgement, the confirmation
	// prompt) checks it; everything else — events, activity rows, history,
	// the archive — behaves exactly as for a spoken turn. Set before the
	// session's first submit and immutable after, so it is read without the
	// lock like wake.
	quiet bool

	// scheduled marks a session a clock started rather than a person — a
	// routine's firing, a reminder's moment, a focus check-in. The return
	// briefing (#150) reads it to answer the one question the wall clock
	// cannot: whether the user is actually here. Counting a reminder that
	// spoke at three in the morning as a sighting would erase the very
	// absence the briefing exists to describe. Set before the session's
	// first submit and immutable after, so it is read without the lock like
	// wake and quiet.
	scheduled bool

	// replay marks a session that only re-speaks a recorded turn (issue
	// #122): no transcript, no model call, and — enforced by claiming
	// committed at birth — nothing ever written to the conversation. It
	// exists so ReplaySpeech can tell "a live exchange holds the floor"
	// (refuse: live speech wins) from "an earlier replay is still playing"
	// (supersede it: the newest click wins). Set under Engine.mu before the
	// session leaves Idle and immutable after.
	replay bool

	// speaker is the turn's streaming speaker, registered at construction so
	// CancelSpeech can ask the component that actually owns playback whether
	// audio is live, instead of inferring it from the session state — the
	// inference that made "stop" a no-op the moment a tool round put the state
	// back in Thinking while sentences were still draining (issue #54).
	// Guarded by Engine.mu. It stays registered after draining: the speaker
	// itself reports drained, so there is nothing to unregister.
	speaker *streamingSpeaker
	// promptAudio marks a confirmation question playing outside any speaker —
	// the direct path speakPrompt takes for a user-defined intent, which asks
	// before the turn has a voice of its own. It exists so "stop" can silence
	// that question too: no speaker is registered for it, and without this
	// flag the one moment Jarvix speaks on an intent turn would be the one
	// moment it could not be stopped. Guarded by Engine.mu.
	promptAudio bool
}

// recordDelta appends one streamed fragment to the turn's running assistant
// text. Called from the streaming goroutine per delta; see streamedMu for why
// this takes its own lock rather than Engine.mu.
func (s *sess) recordDelta(text string) {
	s.streamedMu.Lock()
	s.streamed.WriteString(text)
	s.streamedMu.Unlock()
}

// streamedText snapshots everything the assistant has streamed this turn so
// far. Called from cancel paths holding Engine.mu (mu → streamedMu, never the
// reverse).
func (s *sess) streamedText() string {
	s.streamedMu.Lock()
	defer s.streamedMu.Unlock()
	return s.streamed.String()
}

// NewEngine wires the engine. logger, registry, and store may be nil (no
// tools, no persistence). Construction restores any persisted conversation
// so a follow-up asked after a daemon restart still has its context.
func NewEngine(provider ai.Provider, transcriber stt.Transcriber, synthesizer tts.Synthesizer,
	recorder audio.Recorder, player audio.Player, registry *tools.Registry,
	store history.Store, bus *Bus, logger *slog.Logger, opts Options) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Intents != nil && opts.IntentRunner == nil {
		opts.IntentRunner = &intent.ExecRunner{Log: logger}
	}
	e := &Engine{
		provider: provider,
		stt:      transcriber,
		tts:      synthesizer,
		recorder: recorder,
		player:   player,
		tools:    registry,
		bus:      bus,
		log:      logger,
		opts:     opts,
		store:    store,
		now:      time.Now,
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
		progressAfter: DefaultToolProgressAfter,
		speech:        newSpeechNormalizer(opts.Lexicon),
		approvals:     make(map[string]bool),
		state:         StateIdle,
	}
	if opts.ConfirmTimer != nil {
		e.timer = opts.ConfirmTimer
	}
	// Idle began when the engine did. Without this a conversation.get before
	// the first session would report a zero time, and a client subtracting it
	// from now would compute fifty-odd years of idleness.
	e.stateSince = e.now()
	e.loadHistory()
	return e
}

// loadHistory restores persisted conversation memory at construction, before
// any session can run. Persistence must never stop the daemon from booting:
// a corrupt or unreadable file downgrades to a warning and an empty history.
// With memory disabled the on-disk history is removed instead, so nothing
// from an earlier configuration lingers on disk.
func (e *Engine) loadHistory() {
	if e.store == nil {
		return
	}
	if e.opts.HistoryTurns <= 0 {
		if err := e.store.Clear(); err != nil {
			e.log.Warn("could not remove persisted conversation history",
				"component", "session", "error", err.Error())
		}
		return
	}
	msgs, lastTurn, err := e.store.Load()
	if err != nil {
		e.log.Warn("conversation history could not be loaded; starting fresh",
			"component", "session", "error", err.Error())
		return
	}
	if len(msgs) == 0 {
		return
	}
	// The configured turn cap may have shrunk since the history was written.
	if max := e.opts.HistoryTurns * 2; len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}
	e.history = msgs
	e.lastTurn = lastTurn
	// The restored head starts the position counter at its own length: the
	// messages the file did not keep are also not displayed, so the base of
	// the window and the base of the count coincide. Confirmation records are
	// not restored here — history.json is the model-context head and carries
	// none (issue #118); they live in the archive, and reopening the
	// conversation (conversation.open) brings them back into the display.
	e.msgCount = len(msgs)
	// The restored head belongs to an archived conversation; reattach so the
	// next turn appends to the same record instead of forking a new one per
	// restart (ADR 0027). A stale or missing pointer degrades to "": the next
	// flush simply starts a fresh conversation.
	if e.opts.Archive != nil {
		e.archiveID = e.opts.Archive.Active()
	}
	e.log.Debug("conversation history loaded", "component", "session", "turns", len(msgs)/2)
}

// State returns the current state and active session id ("" when idle).
func (e *Engine) State() (State, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	return e.state, id
}

// Phase is everything a surface needs to say what Jarvix is doing right now
// and for how long: the state, whose session it belongs to, when the state
// began, and the tool call in flight if there is one.
type Phase struct {
	State     State
	SessionID string
	// Since is when the current state began, on the engine's clock.
	Since time.Time
	// Tool is the executing tool call's name, "" when none is running, and
	// ToolDetail its progress label ("Consulting claude…") for the tools that
	// publish one.
	Tool       string
	ToolDetail string
}

// Phase reports the current phase as one consistent read. It is one method
// rather than four accessors because a client rendering a "Thinking · 12s"
// line from separate calls could pick up a state from one moment and a start
// time from the next, and then quote a duration that never happened.
func (e *Engine) Phase() Phase {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	return Phase{
		State:      e.state,
		SessionID:  id,
		Since:      e.stateSince,
		Tool:       e.runningTool,
		ToolDetail: e.runningToolDetail,
	}
}

// StartSession begins a new session. If a session is already active — for
// example Jarvix is mid-sentence — it is cancelled immediately so the new
// interaction can begin: interruption must feel instant.
func (e *Engine) StartSession() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startSessionLocked()
}

// startSessionLocked is StartSession's body, split out so a caller that has
// already decided something under the lock — SubmitText choosing between a new
// turn and a pending confirmation (text.go) — can start the session without
// dropping the lock in between. Releasing it would open a window in which the
// state it just read no longer holds.
//
// Every refusal a new session can meet lives here rather than in StartSession,
// so a second entry point cannot be added that quietly skips one: typed input
// (SubmitText) is refused by a shutting-down engine on exactly the same terms
// as a spoken turn.
func (e *Engine) startSessionLocked() (string, error) {
	if e.shuttingDown {
		return "", fmt.Errorf("the daemon is shutting down")
	}
	if e.reconfiguring {
		// Milliseconds at most: Reconfigure only drains goroutine tails.
		return "", fmt.Errorf("new settings are being applied; try again in a moment")
	}
	if e.current != nil {
		e.cancelLocked("interrupted by new session")
	}
	e.counter++
	ctx, cancel := context.WithCancel(context.Background())
	e.current = &sess{
		id:      fmt.Sprintf("s%d", e.counter),
		ctx:     ctx,
		cancel:  cancel,
		started: time.Now(),
		// The timing marks share the engine's injectable clock, so tests can
		// drive the latency arithmetic (issue #72) the way they drive the
		// follow-up window.
		timings: timings{now: e.now},
	}
	e.log.Info("session started", "component", "session", "session_id", e.current.id)
	return e.current.id, nil
}

// StartScheduledSession begins a session for a schedule's clockfire (ADR
// 0032). Two deliberate differences from StartSession, both consequences of
// nobody having spoken: a clockfire must never interrupt — an active session
// is a refusal here (the caller reports a skipped firing), where a spoken
// activation would cancel it — and with announce false the session is marked
// quiet, so its outcome lands in the activity feed and the finish
// notification rather than as a voice at whatever hour the schedule names.
func (e *Engine) StartScheduledSession(announce bool) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil {
		return "", fmt.Errorf("a session is already active")
	}
	id, err := e.startSessionLocked()
	if err != nil {
		return "", err
	}
	e.current.quiet = !announce
	// A clockfire is the daemon talking to itself, however loudly: it is
	// never evidence that the user came back (#150).
	e.current.scheduled = true
	return id, nil
}

// StartVoice begins microphone capture for the active session. While a tool
// confirmation is pending it captures the user's answer instead: the same
// Listening → Transcribing path runs, but the transcript resolves the
// confirmation (yes/no) rather than starting a new question.
func (e *Engine) StartVoice() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	replyCapture := e.state == StateAwaitingConfirmation && e.pending != nil
	if err := e.setStateLocked(StateListening); err != nil {
		return err
	}
	if replyCapture {
		// The user is engaging: the confirmation timeout no longer applies,
		// and the transcript gates below must wait for the reply, not reuse
		// the original question's.
		e.pending.engaged = true
		s.replyCapture = true
		s.transcript = ""
		s.transcriptReady = false
		s.submitted = false
	}
	rec, err := e.recorder.Start(s.ctx)
	if err != nil {
		e.failLocked(s, "audio", err)
		return err
	}
	s.recording = rec
	s.voiceStarted = time.Now()
	e.publish(Event{Type: "recording.started", Data: map[string]any{"session_id": s.id}})
	return nil
}

// StopVoice ends capture and starts transcription in the background. A
// capture shorter than Options.MinRecording is discarded as an accidental
// activation: no transcription, no error — the session just ends quietly,
// and discarded=true tells the caller to skip its follow-up submit.
func (e *Engine) StopVoice() (discarded bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil || s.recording == nil {
		return false, fmt.Errorf("not recording")
	}
	if held := time.Since(s.voiceStarted); held < e.opts.MinRecording {
		e.log.Info("recording discarded as accidental", "component", "session",
			"session_id", s.id, "held_ms", held.Milliseconds(),
			"min_ms", e.opts.MinRecording.Milliseconds())
		e.cancelLocked(fmt.Sprintf("recording too short (%dms, minimum %dms)",
			held.Milliseconds(), e.opts.MinRecording.Milliseconds()))
		return true, nil
	}
	if err := e.setStateLocked(StateTranscribing); err != nil {
		return false, err
	}
	rec := s.recording
	s.recording = nil
	// The latency budget starts here: everything a user perceives as "how long
	// until it answers" is measured from the key release, not from the moment
	// some later stage happened to begin.
	s.timings.markCaptureStop()
	e.publish(Event{Type: "recording.stopped", Data: map[string]any{"session_id": s.id}})
	e.active.Go(func() { e.transcribe(s, rec) })
	return false, nil
}

// Submit marks the session ready to proceed to the assistant. With text, the
// transcript step is skipped entirely (the `jarvix ask` path); without text
// the session proceeds once transcription finishes. While a tool
// confirmation is pending, submitted text is the user's answer to it —
// affirmative approves, anything else declines.
func (e *Engine) Submit(text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submitLocked(text)
}

// submitLocked is Submit's body; see startSessionLocked for why the split
// exists. Every text submission — `jarvix ask`, `session.submit`, the
// conversation window's composer — lands here, so there is exactly one place
// that decides a typed string is an answer to a pending confirmation rather
// than a new question.
func (e *Engine) submitLocked(text string) error {
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	if text != "" && e.state == StateAwaitingConfirmation && e.pending != nil {
		e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": text}})
		e.resolveConfirmationLocked(isAffirmative(text), "text")
		return nil
	}
	if text != "" {
		s.transcript = text
		s.transcriptReady = true
		s.timings.markTranscript()
		e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": text}})
	}
	s.submitted = true
	e.maybeThinkLocked(s)
	return nil
}

// Cancel stops everything associated with the current interaction.
func (e *Engine) Cancel() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil // nothing to cancel; not an error
	}
	e.cancelLocked("cancelled")
	return nil
}

// CancelSpeech stops spoken output whenever any is actually playing, and
// reports whether it stopped anything. It is the one mechanism every stop path
// reaches — the spoken "stop" intent (ADR 0017), the speech.cancel IPC method,
// and any client binding — so there is exactly one place that decides whether
// there is something to stop.
//
// That decision is made by asking the turn's speaker, never by reading the
// session state. The state describes what the turn is doing; the speaker owns
// the playback stream and knows whether the device is busy — and the two
// disagree routinely, because a mid-answer tool round puts the session back in
// Thinking or Responding while queued sentences are still draining (issue
// #54). Guarding on `state == Speaking`, as this method used to, made "stop"
// do nothing precisely when Jarvix was being long-winded.
//
// Stopping speech ends the turn: the user has heard enough, and a turn that
// silently kept running tools and streaming text it would never say has no one
// listening for it. The teardown mirrors the interruption path — the session
// context is cancelled (which is what kills playback immediately, without
// waiting on synthesis in flight), the state unwinds through Cancelling, and
// session.finished is published, so the turn always reaches a terminal state.
//
// A pending tool confirmation is abandoned, exactly as an interruption
// abandons it: the question is silenced with everything else, tool.declined is
// recorded so the audit trail never shows a question without an answer, and
// the command does not run. "Stop" while being asked "should I run this?" is
// the emphatic no — it is even in the decline vocabulary for spoken replies.
//
// With nothing playing this is a reported no-op, not an error: false, a debug
// line, and an untouched session. The debug line matters — a silently ignored
// stop is how issue #54 stayed invisible.
func (e *Engine) CancelSpeech() (stopped bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		e.log.Debug("speech cancel found no session", "component", "session")
		return false
	}
	live, announced := s.promptAudio, false
	if sp := s.speaker; sp != nil {
		spLive, spAnnounced := sp.speaking()
		live = live || spLive
		announced = spAnnounced
	}
	if !live {
		e.log.Debug("speech cancel found nothing playing", "component", "session",
			"session_id", s.id, "state", string(e.state))
		return false
	}
	s.cancel()
	e.clearPendingLocked("interrupted")
	if e.state.Active() {
		e.forceStateLocked(StateCancelling)
		e.forceStateLocked(StateIdle)
	}
	// The tts.finished bookend is owed only if tts.started was published: a
	// turn whose only audio so far was an aside (a confirmation question, a
	// progress reassurance) emits neither.
	if announced {
		e.publish(Event{Type: "tts.finished", Data: map[string]any{"session_id": s.id, "interrupted": true}})
	}
	// "Stop" ends the turn early, and an early end is an interruption of the
	// exchange even though the session finishes normally (issue #117): the
	// question and the partial answer must survive into the next turn, or a
	// follow-up to a stopped answer meets the same amnesia the incident log
	// shows for push-to-talk interruption.
	committed := e.commitInterruptedLocked(s)
	// The abandoned confirmation's record follows even when no exchange was
	// committable (issue #118) — see cancelLocked for why.
	committed = e.finalizeConfRecordsLocked() || committed
	e.log.Info("speech cancelled", "component", "session", "session_id", s.id)
	e.finishLocked(s)
	if committed {
		// think()'s tail sees the dead context and unwinds without writing,
		// so the commit is persisted here, tracked for the shutdown drain.
		e.active.Go(func() {
			e.persistHistory()
			e.persistArchive()
		})
	}
	return true
}

// ---------------------------------------------------------------- internals

// setStateLocked performs a validated transition and publishes state.changed.
func (e *Engine) setStateLocked(to State) error {
	if !CanTransition(e.state, to) {
		return invalidTransition(e.state, to)
	}
	e.state = to
	e.stateSince = e.now()
	if to == StateIdle {
		// No session, no tool in flight. executeTool clears this when the call
		// returns, but a tool blocked inside a cancelled context returns late
		// or never — and every session-ending path (finish, cancel, failure)
		// passes through here, so this is the one place that catches all of
		// them. A surface left saying "Consulting claude" after the turn ended
		// would be reporting work that belongs to nothing (issue #158).
		e.runningTool, e.runningToolDetail = "", ""
	}
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	e.log.Debug("state changed", "component", "session", "state", string(to), "session_id", id)
	// since_ms is the phase's start on the daemon's clock, so every surface
	// counts the same wait from the same instant (issue #158). Additive: a
	// client that ignores it sees exactly the payload it always saw.
	e.publish(Event{Type: "state.changed", Data: map[string]any{
		"state": string(to), "session_id": id,
		"since_ms": e.stateSince.UnixMilli()}})
	return nil
}

// advance transitions on behalf of a background stage. It returns false in two
// situations that could not be more different, and telling them apart is the
// whole point (issue #55):
//
//   - Superseded or cancelled: the user interrupted, or the session ended.
//     This is the common case and it is fine — the cancel path already
//     published the events, so this path stays quiet (and allocation-free:
//     interruption is on the hot path).
//
//   - Refused: the session is live but the transition is not in the table.
//     That is a programming error which has just cost the user their turn,
//     and it used to be *silent* — two real bugs (issue #52) ran for days
//     behind exactly this indistinguishable false, one of them leaving the
//     session wedged with no error, no session.finished, and no answer. A
//     refusal is therefore loud (an error log naming from, to, and session)
//     and terminal: the session fails properly, so the caller's quiet unwind
//     — correct for supersession — can never again strand a live turn in a
//     non-terminal state with nothing published.
//
// Either way the caller must stop its work; it needs no second return value
// because after a refusal the session is already failed and there is nothing
// left for it to report.
func (e *Engine) advance(s *sess, to State) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != s || s.ctx.Err() != nil {
		return false
	}
	from := e.state
	if err := e.setStateLocked(to); err != nil {
		e.log.Error("state transition refused", "component", "session",
			"session_id", s.id, "from", string(from), "to", string(to))
		e.failLocked(s, "session", err)
		return false
	}
	return true
}

// forceStateLocked performs a transition for a caller with no one to hand the
// error to — teardown and resume paths whose callers used to write `_ =`. The
// legality of these transitions is structural (every active state may reach
// Cancelling, Cancelling and Error reach Idle, AwaitingConfirmation reaches
// its resume states), so a refusal here is a programming error; it is logged
// at error level rather than swallowed, because a refused transition must
// never be silent (issue #55). The caller carries on regardless: these are
// paths where stopping halfway would leave more wreckage than proceeding.
func (e *Engine) forceStateLocked(to State) {
	from := e.state
	if err := e.setStateLocked(to); err != nil {
		id := ""
		if e.current != nil {
			id = e.current.id
		}
		e.log.Error("state transition refused", "component", "session",
			"session_id", id, "from", string(from), "to", string(to))
	}
}

func (e *Engine) publish(ev Event) {
	if e.bus != nil {
		e.bus.Publish(ev)
	}
}

// cancelLocked tears the current session down through Cancelling → Idle.
//
// A cancelled turn is not a forgotten one (issue #117): whatever exchange the
// session was carrying — the user's question and any partial answer — is
// committed marked interrupted *before* session.cancelled is published, so a
// client that has seen the event can already find the exchange behind the
// archive read barrier (SyncArchive, issue #115), exactly as session.finished
// promises for a completed turn.
func (e *Engine) cancelLocked(reason string) {
	s := e.current
	if s == nil {
		return
	}
	s.cancel()
	e.clearPendingLocked("interrupted")
	if s.recording != nil {
		rec := s.recording
		s.recording = nil
		// Recording teardown can block briefly; do not hold the lock for it.
		// Tracked all the same: it is a subprocess and a file handle, and a
		// shutdown that raced it would leave both behind.
		e.active.Go(rec.Cancel)
	}
	committed := e.commitInterruptedLocked(s)
	// A confirmation resolved (or abandoned above) by a turn that committed
	// no exchange still owes the archive its record — the approval may have
	// executed something (issue #118). No-op when the interrupted commit
	// just claimed the records, so nothing is ever recorded twice.
	committed = e.finalizeConfRecordsLocked() || committed
	// A session cancelled before it ever left Idle (started, never advanced)
	// has no transition to make — that is a quiet nothing, not a refusal.
	if e.state.Active() {
		e.forceStateLocked(StateCancelling)
		e.forceStateLocked(StateIdle)
	}
	e.publish(Event{Type: "session.cancelled", Data: map[string]any{"session_id": s.id, "reason": reason}})
	e.log.Info("session cancelled", "component", "session", "session_id", s.id,
		"reason", reason, "duration_ms", time.Since(s.started).Milliseconds())
	e.current = nil
	if committed {
		// The cancelled session's own tail unwinds without writing (its stage
		// goroutines see the dead context and return), so the interrupted
		// commit needs a writer of its own — tracked in e.active, like the
		// tail it replaces, so Shutdown's drain waits for it (#29).
		e.active.Go(func() {
			e.persistHistory()
			e.persistArchive()
		})
	}
}

// finishLocked completes the session normally.
func (e *Engine) finishLocked(s *sess) {
	if e.current != s {
		return
	}
	if e.state.Active() {
		if err := e.setStateLocked(StateIdle); err != nil {
			// A finish from a state with no legal way to Idle is a programming
			// error — but it must not leave the engine wedged in an active
			// state with no session, which is unrecoverable without a restart.
			// Every active state can reach Cancelling and Cancelling reaches
			// Idle, so the fallback route always lands (issue #55).
			e.log.Error("state transition refused", "component", "session",
				"session_id", s.id, "from", string(e.state), "to", string(StateIdle))
			e.forceStateLocked(StateCancelling)
			e.forceStateLocked(StateIdle)
		}
	}
	e.publishTimings(s)
	e.publish(Event{Type: "session.finished", Data: map[string]any{"session_id": s.id}})
	e.log.Info("session finished", "component", "session", "session_id", s.id,
		"duration_ms", time.Since(s.started).Milliseconds())
	s.cancel()
	e.current = nil
}

// publishTimings reports the latency budget of a session that got far enough
// to have one. It goes out before session.finished, which the bus guarantees
// is a session's last event, so a client can attribute the numbers without
// racing the end of the session.
func (e *Engine) publishTimings(s *sess) {
	report := s.timings.report()
	if len(report) == 0 {
		return
	}
	// Log the stages in pipeline order so a journal line reads like the
	// pipeline, and every key matches the event and the CLI exactly.
	args := []any{"component", "session", "session_id", s.id}
	for _, stage := range StageOrder {
		if v, ok := report[stage]; ok {
			args = append(args, stage, v)
		}
	}
	e.log.Info("session timings", args...)

	data := make(map[string]any, len(report)+1)
	for k, v := range report {
		data[k] = v
	}
	data["session_id"] = s.id
	e.publish(Event{Type: "session.timings", Data: data})
}

// fail reports a stage failure and ends the session. Cancellation is not a
// failure: if the session's context is already done, the cancel path has
// spoken for the outcome.
func (e *Engine) fail(s *sess, stage string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failLocked(s, stage, err)
}

func (e *Engine) failLocked(s *sess, stage string, err error) {
	if e.current != s || s.ctx.Err() != nil {
		return
	}
	e.clearPendingLocked("error")
	e.log.Error("session failed", "component", stage, "session_id", s.id, "error", err.Error())
	if CanTransition(e.state, StateError) {
		e.forceStateLocked(StateError)
	}
	e.publish(Event{Type: "error", Data: map[string]any{
		"session_id": s.id, "stage": stage, "message": err.Error(),
	}})
	// From Error the way down is always legal; the guard covers a failure
	// raised before the session ever left Idle (an empty text submission),
	// where there is no transition to make.
	if e.state.Active() {
		e.forceStateLocked(StateIdle)
	}
	// A failure has a latency story too — often the interesting one, because
	// the stage it died in is the stage that ran long.
	e.publishTimings(s)
	e.publish(Event{Type: "session.finished", Data: map[string]any{"session_id": s.id}})
	s.cancel()
	e.current = nil
	// A failed turn commits no exchange, but any confirmation it resolved
	// still happened — and if it was approved, the command already ran
	// before the failure. The record is staged standalone (issue #118) and
	// persisted by a writer of its own: the dead session's tail sees the
	// cancelled context and unwinds without writing, exactly as the
	// interrupted-commit paths found (#117). Tracked in e.active so the
	// shutdown drain waits for it (#29).
	if e.finalizeConfRecordsLocked() {
		e.active.Go(e.persistArchive)
	}
}

// NothingHeardDefaultReason is what a turn reports when the engine returned no
// words and did not say why — another transcriber, or whisper genuinely
// finding nothing in audio that did carry signal.
const NothingHeardDefaultReason = "no speech was recognised"

// nothingHeardLocked ends a session whose capture produced no words.
//
// This is deliberately *not* failLocked (issue #191). A microphone that heard
// nothing is the system working: it is the outcome for a chord tapped by
// accident, for a room that stayed quiet, and — since the STT adapter learned
// to discard whisper's hallucinations — for every capture that would once
// have become a sentence the user never said. Reported as an error it lights
// the urgent chip on the bar, the red banner in the conversation window and a
// "Jarvix hit a problem" notification, and it holds them until the next
// session; a user whose microphone is unplugged then sees a fault report per
// press. It is a quiet nothing, and it ends like one.
//
// It is not silent, though. session.nothing_heard carries the reason —
// "the capture had no voiced audio (peak -inf dBFS, floor -72 dBFS)" — so the
// activity feed can show that a capture happened and produced nothing, which
// is exactly what a user debugging their microphone needs and exactly what an
// invented transcript denied them.
func (e *Engine) nothingHeardLocked(s *sess, reason string) {
	if e.current != s || s.ctx.Err() != nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = NothingHeardDefaultReason
	}
	// A confirmation cannot stand here — a voice reply to one resolves in the
	// branch above and never reaches this check — but the invariant that no
	// question outlives its session is worth keeping unconditionally rather
	// than by argument.
	e.clearPendingLocked("nothing_heard")
	e.log.Info("capture produced no transcript", "component", "stt",
		"session_id", s.id, "reason", reason)
	// Published *before* the return to idle, unlike session.cancelled. The
	// conversation window closes its pending row the moment the state goes
	// idle, and a cancelled turn can afford that because the record it
	// re-requests carries the interrupted exchange's own annotation. Here
	// there is no record — that is the whole point — so a row closed first
	// would simply vanish, and a question that disappears without a word is
	// the failure this issue is about wearing different clothes.
	e.publish(Event{Type: "session.nothing_heard", Data: map[string]any{
		"session_id": s.id, "reason": reason,
	}})
	if e.state.Active() {
		if err := e.setStateLocked(StateIdle); err != nil {
			// The same fallback finishLocked takes (issue #55): every active
			// state reaches Cancelling and Cancelling reaches Idle, so the
			// engine can never be left active with no session.
			e.log.Error("state transition refused", "component", "session",
				"session_id", s.id, "from", string(e.state), "to", string(StateIdle))
			e.forceStateLocked(StateCancelling)
			e.forceStateLocked(StateIdle)
		}
	}
	// The turn's latency story is published like any other, and it carries
	// the reason too: `jarvix status --last` is where someone looks after
	// pressing the key and getting nothing.
	s.timings.noteNothingHeard(reason)
	e.publishTimings(s)
	e.publish(Event{Type: "session.finished", Data: map[string]any{"session_id": s.id}})
	s.cancel()
	e.current = nil
	// No exchange is committed: there is no question to remember, and that
	// is the whole point. A confirmation this session resolved still owes the
	// archive its record, on failLocked's terms (issue #118).
	if e.finalizeConfRecordsLocked() {
		e.active.Go(e.persistArchive)
	}
}

// maybeThinkLocked advances to Thinking once transcript and submission have
// both arrived, whichever order they came in.
func (e *Engine) maybeThinkLocked(s *sess) {
	if !s.transcriptReady || !s.submitted || e.current != s {
		return
	}
	if s.replyCapture {
		// The transcript answers a pending tool confirmation, not a new
		// question. An empty or unrecognised reply declines: the safe
		// reading of anything that is not a clear yes.
		s.replyCapture = false
		e.resolveConfirmationLocked(isAffirmative(s.transcript), "voice")
		return
	}
	if e.state != StateIdle && e.state != StateTranscribing {
		// A duplicate submission (e.g. a stray session.submit while the
		// session is already thinking or awaiting confirmation) must not
		// start a second think round.
		return
	}
	if s.wake {
		// The wake word is at the front of the transcript because the pre-roll
		// deliberately contains it. Nothing downstream wants it: the intent
		// router matches whole utterances, the model reads it as the user
		// addressing a third party, and the conversation history would carry
		// it into every follow-up.
		s.transcript = stripWakeWord(s.transcript, e.opts.WakeWord, e.opts.WakeAliases)
	}
	if strings.TrimSpace(s.transcript) == "" {
		e.nothingHeardLocked(s, s.transcriptReason)
		return
	}
	// Snapshot the processed transcript for the turn's whole life. Everything
	// that records this turn — the normal commit, an interrupted commit, the
	// provider request — reads this copy, because s.transcript itself is
	// overwritten by a voice reply to a tool confirmation and the record must
	// remember the question, never the "yes" that approved a step of it.
	s.userText = s.transcript
	// The deterministic intent router gets the transcript first (ADR 0017).
	// A hit executes a local action and finishes the session without ever
	// opening a provider request; a miss costs one map lookup and falls
	// through to think() unchanged.
	if e.routeIntentLocked(s) {
		return
	}
	if err := e.setStateLocked(StateThinking); err != nil {
		e.failLocked(s, "session", err)
		return
	}
	e.active.Go(func() { e.think(s) })
}

// transcribe runs STT on a finished recording, then hands over to the
// assistant when the session has been submitted.
func (e *Engine) transcribe(s *sess, rec audio.Recording) {
	clip, err := rec.Stop()
	if err != nil {
		e.fail(s, "audio", err)
		return
	}
	defer func() { _ = os.Remove(clip.WAVPath) }()

	start := time.Now()
	events, err := e.stt.Transcribe(s.ctx, stt.AudioInput{
		WAVPath:    clip.WAVPath,
		SampleRate: clip.SampleRate,
		Channels:   clip.Channels,
	})
	if err != nil {
		e.fail(s, "stt", err)
		return
	}
	for ev := range events {
		switch ev.Type {
		case stt.EventPartial:
			e.publish(Event{Type: "transcript.partial", Data: map[string]any{"session_id": s.id, "text": ev.Text}})
		case stt.EventFinal:
			e.log.Info("transcription finished", "component", "stt",
				"session_id", s.id, "duration_ms", time.Since(start).Milliseconds())
			s.timings.markTranscript()
			e.mu.Lock()
			if e.current == s {
				s.transcript = ev.Text
				s.transcriptReady = true
				s.transcriptReason = ev.Reason
				// No transcript event for a capture that produced no words.
				// The old behaviour published an empty one, which every
				// surface renders as the user having said nothing out loud —
				// a blank speech bubble is still a claim about what happened
				// (issue #191). The nothing-heard path below says it plainly
				// instead.
				if strings.TrimSpace(ev.Text) != "" {
					e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": ev.Text}})
				}
				e.maybeThinkLocked(s)
			}
			e.mu.Unlock()
		case stt.EventError:
			if s.ctx.Err() == nil {
				e.fail(s, "stt", ev.Err)
			}
			return
		}
	}
}

// maxToolRounds bounds the tool-call loop so a model that keeps requesting
// tools without answering cannot loop forever.
const maxToolRounds = 6

// think drives the assistant: it streams a response — speaking each sentence
// aloud as soon as it is complete — and whenever the model requests tools it
// executes them (under the session context), feeds the results back, and
// continues, until the model produces a final answer or the round budget is
// exhausted. Prior exchanges are carried in as conversation context.
func (e *Engine) think(s *sess) {
	// The user is demonstrably here, and this is where the return briefing
	// finds that out (#150, ADR 0050). Before anything else in the turn, so
	// the absence is measured against the previous sighting rather than
	// against this one.
	e.arrive(s)
	// Desktop context is gathered here and nowhere earlier: this function is
	// the one path that opens a provider request, so a transcript the intent
	// router already claimed never waits on hyprctl or wl-paste (ADR 0019).
	snapshot := e.gatherContext(s)
	// The knowledge base is consulted on the same terms (ADR 0025): only a
	// turn that reaches the provider pays, and what it pays is one stat(2).
	remembered := e.gatherMemory(s)
	// The taught vocabulary likewise (issue #129): one stat(2), standing
	// knowledge, and with nothing taught the turn is byte-identical.
	taught := e.gatherVocabulary(s)
	// Feed values likewise (ADR 0031): cached readings only, never a fetch —
	// a turn must not wait on a feed command.
	feeds := e.gatherKnowledge(s)
	messages := e.conversationMessages(s.userText, snapshot, remembered.Message, taught.Message, feeds.Message)

	var toolDefs []ai.ToolDef
	if e.tools != nil && !e.tools.Empty() {
		toolDefs = e.tools.Defs()
	}

	var speaker *streamingSpeaker
	if e.opts.SpeakResponses && e.tts != nil && !s.quiet {
		speaker = newStreamingSpeaker(e, s)
	}
	// What this turn has committed to saying out loud, carried through the tool
	// loop so a call that does not run can be reported to the model alongside
	// the preamble it has to take back (issue #52).
	turn := spokenTurn{speaker: speaker}

	e.publish(Event{Type: "assistant.started", Data: map[string]any{"session_id": s.id, "provider": e.provider.Name()}})

	finalText := ""
	// How many tool calls the model requested this turn — attempts, not
	// successes, because a denied or declined call is still the model trying
	// to act. Zero on the final answer is the fact the activity feed's
	// text-only marker states (issue #70): an answer that claims action while
	// this stayed at zero is the model narrating work it never asked to do.
	toolCalls := 0
	for round := 0; round < maxToolRounds; round++ {
		if round > 0 && speaker != nil {
			// Each provider round is a new speech turn (issue #120): the model
			// went away to work and is answering again, and what it says now
			// is the message the user is actually waiting on. The first
			// sentence this round commits to the voice supersedes whatever
			// earlier rounds still have queued unplayed — strictly cross-turn,
			// decided at commit (a round that streams no text supersedes
			// nothing; see streamingSpeaker.nextTurn and enqueue).
			speaker.nextTurn()
		}
		text, calls, ok := e.streamOnce(s, ai.ChatRequest{
			Model:       e.opts.Model,
			MaxTokens:   e.opts.MaxTokens,
			Temperature: e.opts.Temperature,
			Messages:    messages,
			Tools:       toolDefs,
		}, speaker)
		if !ok {
			e.abortSpeaker(speaker) // failed/cancelled/superseded — already reported
			return
		}
		if len(calls) == 0 {
			finalText = text
			break
		}
		if round == maxToolRounds-1 {
			e.abortSpeaker(speaker)
			e.fail(s, "assistant", fmt.Errorf("stopped after %d tool rounds without a final answer", maxToolRounds))
			return
		}

		// The model wants tools. Record its request, gate and run each call,
		// append results, and loop. The permission gate (ADR 0014) sits in
		// front of every execution: denied and declined calls still produce
		// a result message, so the model can answer gracefully instead of
		// the session dying.
		messages = append(messages, ai.Message{Role: ai.RoleAssistant, Content: text, ToolCalls: calls})
		// streamOnce has already flushed this round's text to the speaker, so
		// by now every word of it is committed to the one playback queue: from
		// the gate's point of view it has been said.
		turn = turn.add(text)
		if text != "" {
			// A round boundary in the mirrored stream, so an interrupted
			// commit does not glue this round's narration to the next round's
			// first word.
			s.recordDelta("\n")
		}
		// The model has stopped answering and gone back to work: the moment
		// the table's Responding → Thinking entry describes.
		e.backToThinking(s)
		for _, call := range calls {
			if s.ctx.Err() != nil {
				e.abortSpeaker(speaker)
				return
			}
			toolCalls++
			result, ok := e.gateAndExecute(s, call, turn)
			if !ok {
				e.abortSpeaker(speaker)
				return
			}
			messages = append(messages, ai.Message{Role: ai.RoleTool, ToolCallID: call.ID, Content: result})
		}
	}

	// The return briefing's offer rides the first answer after an absence
	// (#150, ADR 0050): one sentence, appended here so it reaches the ear and
	// the overlay as part of the answer the user actually asked for — never as
	// a separate turn nobody invited. After the loop and before the event, so
	// what is published is what is spoken; before speaker.close(), so it is
	// spoken at all.
	//
	// recordedText is what the conversation keeps, and it diverges from what
	// was said in exactly one case: with briefing.speak_on_return the whole
	// account is spoken here, and a briefing is transient — recording it would
	// put into conversation memory precisely what the explicit-ask path takes
	// care to keep out of it.
	recordedText := finalText
	if finalText != "" {
		if offer, transient := e.offerLine(s); offer != "" {
			finalText = strings.TrimSpace(finalText) + " " + offer
			if !transient {
				recordedText = finalText
			}
			if speaker != nil {
				speaker.speak(offer)
			}
		}
	}

	e.publish(Event{Type: "assistant.finished", Data: map[string]any{
		"session_id": s.id, "content": finalText, "tool_calls": toolCalls}})
	if finalText == "" {
		e.abortSpeaker(speaker)
		e.fail(s, "assistant", fmt.Errorf("the assistant returned an empty response"))
		return
	}

	// Wait for the last sentence to finish speaking, then record the exchange
	// so the next turn has this one as context.
	if speaker != nil {
		if err := speaker.close(); err != nil {
			if s.ctx.Err() == nil {
				e.fail(s, "tts", err)
			}
			return
		}
	}
	e.commitTurn(s, s.userText, recordedText)

	e.mu.Lock()
	e.finishLocked(s)
	e.mu.Unlock()

	// Persist only after the session has fully finished, off the lock path:
	// disk I/O adds zero latency to the spoken exchange. The archive flush
	// rides the same tail for the same reason — and, running inside e.active,
	// the same shutdown drain (#29).
	e.persistHistory()
	e.persistArchive()
}

// streamOnce runs one provider turn, forwarding text deltas to the overlay
// and, when a speaker is present, feeding complete sentences to it as they
// form. ok is false when it already handled a terminal condition.
func (e *Engine) streamOnce(s *sess, req ai.ChatRequest, speaker *streamingSpeaker) (text string, calls []ai.ToolCall, ok bool) {
	start := time.Now()
	events, err := e.provider.Chat(s.ctx, req)
	if err != nil {
		e.fail(s, "assistant", err)
		return "", nil, false
	}
	var full strings.Builder
	var sc sentencer
	responded := false
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			if !responded {
				s.timings.markFirstDelta()
				if !e.advance(s, StateResponding) {
					return "", nil, false
				}
				responded = true
				e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": ""}})
			}
			full.WriteString(ev.Content)
			// Mirrored onto the session so an interruption can commit what
			// was already streamed (issue #117) — think()'s locals die with
			// the goroutine, and the incident lost a fully-streamed question
			// for exactly that reason.
			s.recordDelta(ev.Content)
			e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": ev.Content}})
			if speaker != nil {
				for _, sentence := range sc.push(ev.Content) {
					speaker.speak(sentence)
				}
			}
		case ai.EventToolCall:
			// A tool call is the provider's first output as much as a token
			// is. Without this mark, a round that narrates nothing would push
			// firstDelta past the confirmation question's audio and the
			// pipeline marks would fall out of order — the arithmetic behind
			// issue #72's negative jarvix_ms.
			s.timings.markFirstDelta()
			calls = append(calls, ev.Call)
		case ai.EventError:
			if s.ctx.Err() == nil {
				e.fail(s, "assistant", ev.Err)
			}
			return "", nil, false
		case ai.EventDone:
		}
	}
	if speaker != nil {
		for _, sentence := range sc.flush() {
			speaker.speak(sentence)
		}
	}
	e.log.Info("assistant turn finished", "component", "assistant", "session_id", s.id,
		"provider", e.provider.Name(), "tool_calls", len(calls),
		"duration_ms", time.Since(start).Milliseconds())
	return strings.TrimSpace(full.String()), calls, true
}

// backToThinking returns the session to Thinking for a tool round whose text
// was streamed but never spoken — the state is Responding, and the next
// round's first delta would otherwise be refused as Responding → Responding
// (the table forbids self-transitions on purpose). This is the code path that
// performs the table's Responding → Thinking entry, which was documented from
// the start but wired to nothing: with speech enabled a complete sentence
// moves the state on to Speaking before the tool call lands, so the gap only
// opened with speech off or a preamble too short for the sentencer — and then
// the turn died silently, which is how it went unnoticed (issues #52/#55).
//
// "A complete sentence moves the state on to Speaking before the tool call
// lands" is an invariant, not a race, and it is held by the speaker's
// announce running synchronously on the goroutine that enqueues the
// sentence (streamOnce, this same goroutine). It used to be a race — the
// claim lived on the speaker's run loop — and losing it was issue #111: a
// round whose
// only sentence reached the speaker at end-of-stream would be yanked
// Responding → Thinking here first, and the speaker's late claim then died as
// Thinking → Speaking. By construction now, Responding here means nothing was
// committed to the voice this round, which is exactly when going back to
// Thinking is right.
//
// Speaking deliberately stays put: audio from the round's sentences is still
// draining, and #52 established that the session remains Speaking while it
// does — the next round re-enters Responding from there. Thinking needs no
// move at all (the round streamed no text).
func (e *Engine) backToThinking(s *sess) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != s || s.ctx.Err() != nil || e.state != StateResponding {
		return
	}
	e.forceStateLocked(StateThinking)
}

// abortSpeaker closes a speaker without treating its result as a fresh error:
// the session already failed or was cancelled, and that path owns the events.
func (e *Engine) abortSpeaker(speaker *streamingSpeaker) {
	if speaker != nil {
		_ = speaker.close()
	}
}

// conversationMessages builds the provider message list for a new turn:
// system prompt, remembered facts, carried-over history (reset if the
// follow-up window lapsed), the desktop context capture, then the new user
// message.
//
// Context sits last-but-one on purpose. It is a system message, so the model
// reads it as ground truth about the machine rather than as something the
// user typed; and it sits *after* the history so that "right now" is
// unambiguous — a capture describes the moment of this question, and placing
// it before older turns would invite the model to read it as their context
// too. It is never committed to history (commitTurn stores the question and
// the answer), so a capture lives exactly one turn.
//
// Remembered facts sit directly after the system prompt, *before* the
// history, for the mirror-image reason (ADR 0025): they are standing
// knowledge, true across every turn of the thread, so they read as part of
// who the assistant is — while the capture, which describes one moment,
// stays adjacent to the question that moment belongs to. Like the capture,
// the block is never committed to history: it is rebuilt fresh each turn, so
// a hand-edit or a forget is reflected on the very next question.
// The taught vocabulary (issue #129) sits beside the remembered facts, after
// them, on identical terms: how the user talks is standing knowledge about
// the user, true for the whole thread, rebuilt fresh each turn — and with
// nothing taught it contributes no message at all, keeping the zero-entry
// prompt byte-identical to one before the feature existed.
// Feed values (ADR 0031) sit with the capture, not with the remembered
// facts: a feed reading describes "right now" — its whole content is a value
// and an age measured at this turn — so like the capture it stays adjacent
// to the question that moment belongs to, and like the capture it is never
// committed to history: rebuilt fresh each turn, so an answer can never
// quote last turn's price with this turn's confidence.
func (e *Engine) conversationMessages(userText string, snapshot desktop.Snapshot, remembered, taught, feeds string) []ai.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opts.FollowUpWindow > 0 && !e.lastTurn.IsZero() &&
		e.now().Sub(e.lastTurn) > e.opts.FollowUpWindow {
		e.log.Debug("conversation history expired", "component", "session",
			"turns", len(e.history)/2)
		e.history = nil
		e.lastTurn = time.Time{}
		// Remembered tool approvals are conversation-scoped: a new thread
		// must ask again. Conversation-scoped allow patterns (#162) go with
		// them for the same reason and by the same sentence: "just this
		// conversation" has to mean this one.
		e.approvals = make(map[string]bool)
		e.grants = nil
		// The archived record ends with the thread; the new one must not
		// append to it (ADR 0027). Nothing is pending to flush here — every
		// commitTurn's staging is flushed on its own session tail — so only
		// the attachment ends; the generation bump tells any flush still in
		// flight not to adopt an id for this new thread.
		e.archiveID = ""
		e.archiveGen++
	}
	msgs := make([]ai.Message, 0, len(e.history)+4)
	if e.opts.SystemPrompt != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: e.opts.SystemPrompt})
	}
	if remembered != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: remembered})
	}
	if taught != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: taught})
	}
	msgs = append(msgs, e.history...)
	if captured := snapshot.Message(); captured != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: captured})
	}
	if feeds != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: feeds})
	}
	msgs = append(msgs, ai.Message{Role: ai.RoleUser, Content: userText})
	return msgs
}

// commitTurn records a completed exchange as context for the next turn, kept
// to the configured number of turns. Intermediate tool traffic is not stored;
// the user question and the assistant's final answer are what carry meaning.
//
// It takes the session because commit-or-not is per-turn state: a cancel that
// raced the tail may already have committed this exchange marked interrupted
// (issue #117), and recording it a second time — once cut, once whole — would
// be worse than either alone. Whoever sets s.committed first wins; both
// versions are truthful about what the user experienced at the moment each
// was written.
func (e *Engine) commitTurn(s *sess, userText, assistantText string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.committed {
		return
	}
	s.committed = true
	e.commitTurnLocked(userText, assistantText, false, s.provenanceRecord())
}

// The wording an interrupted exchange carries into the record (issue #117).
// The mark lives in the assistant half's *text* — beside the machine-readable
// flag the archive keeps — because text is the one channel every consumer
// already reads: the model sees why the thread's last answer stops
// mid-thought (and never claims the exchange is missing), the window and
// `jarvix conversations show` render it without new plumbing, and a reopened
// conversation carries it back into context through adoptableMessages
// untouched. Square brackets keep it visibly an annotation, not speech.
const (
	// interruptedMidAnswer follows partial answer text.
	interruptedMidAnswer = "[interrupted — the user cut this answer off here]"
	// interruptedNoAnswer stands alone when the turn was cut before any
	// answer streamed. It is the whole assistant half rather than an empty
	// string, because providers reject empty messages and the history's
	// user/assistant pairing must hold (the "Done." precedent in runIntent).
	interruptedNoAnswer = "[interrupted — the user cut in before any answer was given]"
)

// commitInterruptedLocked commits the exchange a dying session was carrying —
// the user's turn and whatever answer had streamed — marked interrupted, into
// working memory and the archive stage. Callers hold e.mu and, when it
// reports true, must arrange persistence off the lock (the dying session's
// own tail will not: its goroutines see the dead context and unwind without
// writing).
//
// It reports false — nothing committed — when the turn was already committed
// (the normal tail won the race), or when there is no user turn to commit: a
// session cancelled while still listening, a too-short accidental tap, a
// reply capture whose transcript is a yes/no and not a question. The one
// commit-worthy thing such sessions could carry, the confirmation they were
// answering, belongs to the session that asked it and is committed there.
func (e *Engine) commitInterruptedLocked(s *sess) bool {
	if s.committed {
		return false
	}
	user := strings.TrimSpace(s.userText)
	if user == "" && s.transcriptReady && !s.replyCapture {
		// Cancelled after the transcript landed but before maybeThinkLocked
		// processed it — the snapshot was never taken, so take it here, with
		// the same wake-word strip processing would have applied.
		t := s.transcript
		if s.wake {
			t = stripWakeWord(t, e.opts.WakeWord, e.opts.WakeAliases)
		}
		user = strings.TrimSpace(t)
	}
	if user == "" {
		return false
	}
	s.committed = true
	assistant := interruptedNoAnswer
	if partial := strings.TrimSpace(s.streamedText()); partial != "" {
		assistant = partial + "\n" + interruptedMidAnswer
	}
	e.commitTurnLocked(user, assistant, true, s.provenanceRecord())
	return true
}

// commitTurnLocked is the one place an exchange enters the record, complete
// or interrupted. Callers hold e.mu and have already claimed s.committed.
//
// prov is what went into the answer (issue #168), nil when the turn consumed
// nothing retrievable. It rides the assistant half both on disk and in the
// live view, anchored to that half's position for the same reason the
// confirmation records are.
func (e *Engine) commitTurnLocked(userText, assistantText string, interrupted bool,
	prov *provenance.Record) {
	// Any confirmations resolved during this exchange are claimed by it now:
	// they were already visible to conversation.get the moment they resolved
	// (issue #118), and here they take their archive position between the
	// exchange's two halves.
	recs := e.unstagedConfRecordsLocked()
	for _, r := range recs {
		r.staged = true
	}
	// The archive is staged before the cap can trim anything, and even with
	// in-memory history disabled: history_turns governs what the model is
	// sent, never what is kept (ADR 0027).
	e.stageArchiveTurnLocked(userText, assistantText, interrupted, recs, prov)
	// Anchored before the counter moves: noteProvenanceLocked reads msgCount
	// as the user half's position and takes the next index for the answer.
	e.noteProvenanceLocked(prov)
	// The position counter advances even when in-memory history is disabled:
	// it counts what was committed to the thread, not what was retained, so
	// record anchors stay comparable to it either way.
	e.msgCount += 2
	if e.opts.HistoryTurns <= 0 {
		e.pruneConfRecordsLocked()
		e.pruneProvenanceLocked()
		return
	}
	e.history = append(e.history,
		ai.Message{Role: ai.RoleUser, Content: userText},
		ai.Message{Role: ai.RoleAssistant, Content: assistantText})
	if max := e.opts.HistoryTurns * 2; len(e.history) > max {
		e.history = append([]ai.Message(nil), e.history[len(e.history)-max:]...)
	}
	e.pruneConfRecordsLocked()
	e.pruneProvenanceLocked()
	e.lastTurn = e.now()
}

// persistHistory writes the conversation to disk so it survives a daemon
// restart. It runs after session.finished, never under mu (beyond a brief
// snapshot), and treats failure as degradation: the engine keeps working in
// memory and warns exactly once. Turn contents are never logged.
func (e *Engine) persistHistory() {
	if e.store == nil || e.opts.HistoryTurns <= 0 || e.persistFailed.Load() {
		return
	}
	e.mu.Lock()
	msgs := append([]ai.Message(nil), e.history...)
	lastTurn := e.lastTurn
	e.mu.Unlock()
	if err := e.store.Save(msgs, lastTurn); err != nil {
		if e.persistFailed.CompareAndSwap(false, true) {
			e.log.Warn("conversation history could not be saved; continuing in memory only",
				"component", "session", "error", err.Error())
		}
		return
	}
	e.log.Debug("conversation history saved", "component", "session", "turns", len(msgs)/2)
}

// Turn is one entry of the current conversation as a client should display
// it. Only what carries meaning for a reader is included: committed
// user/assistant exchanges, the resolved confirmation records between them
// (issue #118), plus the in-flight user question — no system prompt, no tool
// traffic.
type Turn struct {
	Role string `json:"role"` // "user", "assistant", or "confirmation"
	Text string `json:"text"`
	// Confirmation carries the resolved permission-gate record when Role is
	// conversations.RoleConfirmation, and is absent on every utterance —
	// additive, so a client that ignores it sees the shape it always saw.
	Confirmation *conversations.Confirmation `json:"confirmation,omitempty"`
	// Provenance is what went into this answer (issue #168): present on an
	// assistant turn that consumed something retrievable, absent everywhere
	// else — including on every turn that used nothing, because absence is
	// the information that nothing was consulted. Additive on the same terms
	// as Confirmation.
	Provenance *provenance.Record `json:"provenance,omitempty"`
}

// Conversation returns the turns of the current conversation, oldest first,
// for the conversation window (the `conversation.get` IPC method). The
// active session's transcript is included as soon as it is known, so a
// window opened mid-session shows the question being answered; the streamed
// answer itself reaches clients via assistant.delta events. The lazy
// follow-up-window reset is deliberately not applied here: this reports what
// happened, not what the next turn will remember. Never nil — an empty
// conversation is an empty slice, so clients always see a JSON array.
//
// Resolved confirmation records are interleaved at their anchors (issue
// #118): a record renders after the first afterMsgs messages of the thread,
// which for a confirmation resolved mid-exchange is between that exchange's
// question and answer — the position the live card occupied. A still-pending
// confirmation is deliberately NOT a turn here: it rides the snapshot's
// `confirmation` field (issue #76) and renders as the live card, so a reopen
// during the wait shows exactly one card and never a history duplicate.
func (e *Engine) Conversation() []Turn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conversationLocked()
}

// conversationLocked is Conversation's body, split out so ReplaySpeech can
// resolve a turn address against the same view under the lock it already
// holds — the address a client sends means a position in exactly this
// rendering, so any second rendering would be a second chance to disagree.
func (e *Engine) conversationLocked() []Turn {
	turns := make([]Turn, 0, len(e.history)+len(e.confRecords)+1)
	// confRecords is ordered by afterMsgs (resolution order within a
	// monotonic counter), so one cursor walks it alongside the history.
	next := 0
	through := func(g int) {
		for next < len(e.confRecords) && e.confRecords[next].afterMsgs <= g {
			turns = append(turns, e.confRecords[next].displayTurn())
			next++
		}
	}
	base := e.msgCount - len(e.history)
	for i, m := range e.history {
		through(base + i)
		// Provenance is anchored to the message's global position, so the
		// retention cap sliding the head cannot move it onto another answer.
		turns = append(turns, Turn{Role: string(m.Role), Text: m.Content,
			Provenance: e.provenanceAtLocked(base + i)})
	}
	// Records anchored at the committed tail — a confirmation whose turn
	// died without committing an exchange around it — come before the
	// in-flight question of the next turn.
	through(e.msgCount)
	if s := e.current; s != nil && s.transcriptReady && strings.TrimSpace(s.transcript) != "" {
		turns = append(turns, Turn{Role: string(ai.RoleUser), Text: s.transcript})
	}
	// And the confirmations the in-flight exchange has already resolved
	// render after its question, exactly where they will sit once it commits.
	through(e.msgCount + 1)
	return turns
}

// ResetConversation clears the carried-over context — in memory and on disk —
// so the next turn starts a fresh thread and a later restart resurrects
// nothing. Remembered tool approvals die with the conversation too: they are
// scoped to it by design and never persist.
//
// With the archive on, this is what "jarvix new archives the current thread"
// amounts to (ADR 0027): the turns are already on disk — they were appended
// as they completed — so the reset only writes whatever the last exchange
// left staged, then ends the attachment. The archived conversation stays,
// listed and reopenable; only the live head is destroyed.
//
// A session in flight is deliberately left alone here — this is the reset
// half of a delete or an intent turn's own reset, where cancelling would be a
// side effect nobody asked for. NewConversation is the entry point that ends
// the session too.
func (e *Engine) ResetConversation() {
	e.reset(false)
}

// NewConversation is the explicit end of a conversation — the one verb the
// `conversation.new` IPC method, the spoken "start a new conversation"
// intent's engine path, the window's New chat button, and the bar menu all
// converge on (issue #117; ADR 0038). Where ResetConversation only forgets,
// this first cancels any session in flight, which commits that session's
// exchange marked interrupted into the *old* thread before the detach — so
// even the turn the user talked over is archived, searchable, and gone from
// the fresh thread's working memory.
func (e *Engine) NewConversation() {
	e.reset(true)
}

// reset is the shared body of ResetConversation and NewConversation.
func (e *Engine) reset(cancelActive bool) {
	e.mu.Lock()
	if cancelActive && e.current != nil {
		// cancelLocked commits the in-flight exchange (marked interrupted)
		// into e.history and the archive stage — both of which the lines
		// below hand to the old conversation and clear — so the ordering
		// here is what puts the interrupted turn in the ending thread rather
		// than the new one.
		e.cancelLocked("new conversation")
	}
	e.history = nil
	e.lastTurn = time.Time{}
	e.approvals = make(map[string]bool)
	e.grants = nil
	// A record still unstaged here belongs to the thread that is ending —
	// stage it standalone before the detach hands the batch over, then clear
	// the display list: the confirmation records die with the head exactly
	// like the turns they sat between, and stay in the archive exactly like
	// them too (issue #118).
	e.finalizeConfRecordsLocked()
	e.confRecords = nil
	// Provenance dies with the head it described, and stays in the archive
	// with the turns it belonged to — the confirmation records' rule.
	e.provRecords = nil
	e.msgCount = 0
	archive, archivedID, pending := e.detachArchiveLocked()
	e.mu.Unlock()
	e.flushArchiveDetached(archive, archivedID, pending)
	e.publish(Event{Type: "conversation.changed", Data: map[string]any{"reason": "reset"}})
	if e.store == nil {
		return
	}
	if err := e.store.Clear(); err != nil {
		e.log.Warn("could not remove persisted conversation history",
			"component", "session", "error", err.Error())
		return
	}
	e.log.Debug("conversation history cleared", "component", "session")
}

// Shutdown stops the engine for good and waits for its work to finish.
//
// It exists because a session is not over when the user thinks it is. The
// exchange is committed to history and session.finished is published, and
// only *then* — off the lock, on the tail of think() — is the conversation
// written to disk, so that disk I/O adds no latency to the spoken answer
// (ADR 0011). Nothing used to wait for that write, so a shutdown landing in
// the gap (a `systemctl --user restart jarvixd`, an update) dropped the last
// exchange from the persisted conversation. Shutdown closes that gap: no new
// session may start, any session in flight is cancelled, and every tracked
// session goroutine — transcribe, think, the recording teardown, and with
// them the history write — is waited for.
//
// The wait is bounded by ctx and Shutdown returns ctx.Err() when it expires:
// a wedged disk must never keep the daemon alive. Callers log what did not
// settle and exit anyway.
//
// Shutdown is idempotent, and on an already-quiescent engine it returns nil
// even for an expired ctx — so a second call is a cheap assertion that
// everything really has stopped.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.shuttingDown = true
	// A session in flight is not worth waiting out: it is a live model stream
	// with a person no longer there to hear it. Cancelling is what makes the
	// drain finite — the stages watch s.ctx and unwind.
	e.cancelLocked("the daemon is shutting down")
	e.mu.Unlock()
	return e.active.Wait(ctx)
}

// InFlight reports how many session goroutines are still running. It is there
// for the shutdown log: when a drain gives up, the count is the difference
// between one stuck history write and a session that never unwound.
func (e *Engine) InFlight() int { return e.active.InFlight() }

// Reconfigure swaps the engine's collaborators and options for a new
// configuration without a daemon restart. It refuses while a session is
// active: adapters are only ever swapped between sessions, never under one —
// a reload that cannot apply keeps the running configuration untouched.
//
// Idle state alone is not enough to swap safely: session goroutines read the
// swapped fields without holding mu, and a finished or cancelled session's
// think()/transcribe() may still be draining after current went nil. So
// Reconfigure briefly blocks new sessions, waits for every tracked session
// goroutine to exit, and only then swaps — making the swap invisible both to
// running code and to the race detector.
func (e *Engine) Reconfigure(provider ai.Provider, transcriber stt.Transcriber,
	synthesizer tts.Synthesizer, recorder audio.Recorder, player audio.Player,
	opts Options) error {
	e.mu.Lock()
	if e.reconfiguring {
		e.mu.Unlock()
		return fmt.Errorf("new settings are already being applied")
	}
	if e.current != nil || e.state != StateIdle {
		e.mu.Unlock()
		return fmt.Errorf("a session is active; the new settings apply once it finishes (run config.reload)")
	}
	e.reconfiguring = true
	e.mu.Unlock()

	// No new session can start now; drain the tails of past sessions. The wait
	// is unbounded on purpose: a reload that gave up early would swap
	// collaborators out from under a goroutine still reading them, which is
	// the race this drain exists to prevent. Shutdown is the bounded caller —
	// there, exiting late is worse than exiting with work outstanding.
	_ = e.active.Wait(context.Background())

	e.mu.Lock()
	defer e.mu.Unlock()
	e.reconfiguring = false
	e.provider = provider
	e.stt = transcriber
	e.tts = synthesizer
	e.recorder = recorder
	e.player = player
	e.opts = opts
	// A lexicon edit is meant to be heard on the next answer, not after a
	// restart, so the normalizer is rebuilt with the rest of the collaborators.
	e.speech = newSpeechNormalizer(opts.Lexicon)

	// Conversation memory follows the new limits immediately, mirroring what
	// loadHistory enforces at construction.
	if opts.HistoryTurns <= 0 {
		e.history = nil
		e.lastTurn = time.Time{}
		if e.store != nil {
			if err := e.store.Clear(); err != nil {
				e.log.Warn("could not remove persisted conversation history",
					"component", "session", "error", err.Error())
			}
		}
	} else if max := opts.HistoryTurns * 2; len(e.history) > max {
		e.history = append([]ai.Message(nil), e.history[len(e.history)-max:]...)
	}
	e.log.Info("engine reconfigured", "component", "session",
		"provider", provider.Name(), "tts", synthesizer.Name(), "model", opts.Model)
	return nil
}
