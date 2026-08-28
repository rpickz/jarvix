package intent

// This file is the monitor-nickname grammar (#180): "call this monitor top",
// "forget the monitor called top", "what are my screens called".
//
// It is a deliberate copy of the window-nickname grammar's shape (#126) down
// to the compile call and the position in New's insertion order, because the
// two features are the same sentence about different furniture and a user who
// has learned one must not have to learn the other. The differences are only
// where the things themselves differ: monitors persist, so there is a forget
// phrase, which windows do not need — a window releases its name by closing.
//
// Nothing here composes an answer. Like the window-name rules these carry a
// flag and hand the spoken words to the seam that owns screen names, because
// nothing about monitors may live in the router's table.

// MonitorNameIntentName identifies the "call this monitor <name>" intent
// (#180) in logs and the intent.executed event.
const MonitorNameIntentName = "monitor.name"

// MonitorForgetIntentName identifies the "forget the monitor called <name>"
// intent (#180).
const MonitorForgetIntentName = "monitor.forget"

// MonitorNamesIntentName identifies the "what are my screens called" listing
// (#180).
const MonitorNamesIntentName = "monitor.names"

// monitorNamePatterns are the utterances that name the screen holding focus.
// A short literal list ending in the one free-text slot, for the window
// table's reason: the name is the user's to choose and cannot be enumerated.
//
// Every one of them says "monitor" or "screen". "Call this top" without the
// noun is far more likely a sentence for the model than an assignment — and
// worse, it is indistinguishable from the window-nickname phrase, which
// would make which thing got named depend on table order rather than on what
// the user said. Ambiguity belongs to the model, never this table.
var monitorNamePatterns = []string{
	"call this monitor {name}",
	"call that monitor {name}",
	"call this screen {name}",
	"call that screen {name}",
	"name this monitor {name}",
	"name this screen {name}",
	"nickname this monitor {name}",
}

// monitorForgetPatterns are the utterances that drop a screen name. They name
// the nickname rather than a screen ("forget the monitor called top", not
// "forget this monitor's name") because the name is what the user knows: the
// screen it points at may be in a bag, which is often exactly why they are
// forgetting it.
var monitorForgetPatterns = []string{
	"forget the monitor called {name}",
	"forget the screen called {name}",
	"forget the monitor named {name}",
	"forget the screen named {name}",
	"stop calling a monitor {name}",
	"stop calling a screen {name}",
}

// monitorNamesPatterns list the screen names. Fully literal — a near-synonym
// is a code change with a test, like every entry of the built-in table.
var monitorNamesPatterns = []string{
	"what are my monitors called",
	"what are my screens called",
	"what are the monitors called",
	"what are the screens called",
	"what did i call my monitors",
	"what did i call my screens",
	"list my monitor names",
	"list my screen names",
}
