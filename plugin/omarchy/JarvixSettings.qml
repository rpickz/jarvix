import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Jarvix settings screen, hosted inside the conversation window. Like every
// other surface it is display-only: it renders the daemon's config.get /
// doctor.get answers and submits changes through config.set / config.reload —
// validation, file rewriting, and hot-reload all happen daemon-side
// (docs/ipc.md, ADR 0015). Secrets arrive as presence booleans; key values
// never reach this file.
//
// Accessibility: every control is reachable with Tab and operable with
// Space/Enter (arrows cycle choices); state and validation are conveyed as
// text, never colour alone; all sizes come from the shell's Style tokens so
// the user's font scale is respected.
Item {
  id: settings

  // The window sets this while the screen is shown; the socket only lives
  // while it is true, so a closed settings screen costs the daemon nothing.
  property bool active: false

  property string fingerprint: ""
  property var fields: []          // config.get fields
  property var secrets: []         // config.get secrets (presence only)
  property var readiness: ({})     // related key → readiness line
  property string generalReadiness: ""
  property var edits: ({})         // key → edited value (string or bool)
  property var fieldErrors: ({})   // key → validation message
  property string banner: ""
  property string bannerDetail: ""

  onActiveChanged: {
    if (active) {
      if (bridge.connected) refresh()
      else bridge.connected = true
    } else {
      bridge.connected = false
    }
  }

  function refresh() {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: 11, method: "config.get" }) + "\n")
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: 12, method: "doctor.get" }) + "\n")
  }

  function editedValue(key, fallback) {
    return key in edits ? edits[key] : fallback
  }

  function recordEdit(key, value) {
    var e = edits
    e[key] = value
    edits = e // reassign so bindings see the change
  }

  function save() {
    var changes = {}
    var any = false
    for (var key in edits) { changes[key] = edits[key]; any = true }
    if (!any) {
      banner = "Nothing to save"
      bannerDetail = ""
      return
    }
    fieldErrors = {}
    bridge.write(JSON.stringify({
      jsonrpc: "2.0", id: 13, method: "config.set",
      params: { changes: changes, fingerprint: fingerprint }
    }) + "\n")
  }

  function reloadFromFile() {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: 14, method: "config.reload" }) + "\n")
  }

  function handleGetResult(result) {
    fingerprint = String(result.fingerprint || "")
    fields = result.fields || []
    secrets = result.secrets || []
  }

  function handleDoctorResult(result) {
    var byKey = {}
    var general = ""
    var checks = result.checks || []
    for (var i = 0; i < checks.length; i++) {
      var c = checks[i]
      if (c.status === "ok") continue
      var line = (c.status === "fail" ? "Problem: " : "Note: ") + c.name
      if (c.detail) line += " — " + c.detail
      if (c.fix) line += "\nFix: " + c.fix
      if (c.related) byKey[c.related] = line
      else general = line
    }
    readiness = byKey
    generalReadiness = general
  }

  function handleSetResult(result) {
    edits = {}
    fieldErrors = {}
    fingerprint = String(result.fingerprint || fingerprint)
    if (result.applied) {
      banner = "Saved and applied — no restart needed"
      bannerDetail = ""
    } else {
      banner = "Saved to config.toml, not applied yet"
      bannerDetail = String(result.reason || "") + "\nPress “Apply now” once Jarvix is idle."
    }
    var restart = result.needs_restart || []
    if (restart.length > 0) {
      bannerDetail += (bannerDetail ? "\n" : "")
        + "Needs a daemon restart: " + restart.join(", ")
        + "\nRun: systemctl --user restart jarvixd"
    }
    refresh()
  }

  function handleSetError(error) {
    // -32001 invalid, -32002 external-edit conflict, -32003 busy (docs/ipc.md).
    if (error.code === -32001) {
      banner = "Not saved — validation failed"
      bannerDetail = ""
      var problems = (error.data && error.data.problems) || []
      var errs = {}
      for (var i = 0; i < problems.length; i++) {
        var msg = String(problems[i])
        var matched = false
        for (var j = 0; j < fields.length; j++) {
          if (msg.indexOf(fields[j].key) === 0) {
            errs[fields[j].key] = msg
            matched = true
            break
          }
        }
        if (!matched) bannerDetail += (bannerDetail ? "\n" : "") + msg
      }
      fieldErrors = errs
    } else if (error.code === -32002) {
      banner = "Not saved — config.toml was edited outside this screen"
      bannerDetail = "Its new values are shown below; your unsaved edits are kept. Review, then Save again."
      refresh() // pulls the new fingerprint; edits survive the refresh
    } else {
      banner = "Not saved"
      bannerDetail = String(error.message || "unknown error")
    }
  }

  function handleReloadResult(result, error) {
    if (error) {
      banner = "Reload failed — the running configuration is unchanged"
      bannerDetail = String(error.message || "")
      var problems = (error.data && error.data.problems) || []
      for (var i = 0; i < problems.length; i++) {
        bannerDetail += "\n" + String(problems[i])
      }
      return
    }
    banner = "Reloaded from config.toml"
    bannerDetail = ""
    var restart = (result && result.needs_restart) || []
    if (restart.length > 0) {
      bannerDetail = "Needs a daemon restart: " + restart.join(", ")
        + "\nRun: systemctl --user restart jarvixd"
    }
    edits = {}
    refresh()
  }

  Socket {
    id: bridge
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method) {
          // A save elsewhere (CLI, another window) moved the config: refresh.
          if (frame.method === "config.changed") settings.refresh()
          return
        }
        switch (frame.id) {
        case 11:
          if (frame.result) settings.handleGetResult(frame.result)
          break
        case 12:
          if (frame.result) settings.handleDoctorResult(frame.result)
          break
        case 13:
          if (frame.error) settings.handleSetError(frame.error)
          else settings.handleSetResult(frame.result || {})
          break
        case 14:
          settings.handleReloadResult(frame.result, frame.error)
          break
        }
      }
    }

    onConnectionStateChanged: {
      if (connected) settings.refresh()
      else if (settings.active) retry.start()
    }
  }

  Timer {
    id: retry
    interval: 2000
    repeat: false
    onTriggered: { if (settings.active && !bridge.connected) bridge.connected = true }
  }

  function reloadLabel(reload) {
    switch (reload) {
    case "live":    return "applies immediately"
    case "idle":    return "applies without restart (when idle)"
    case "restart": return "needs daemon restart"
    default:        return ""
    }
  }

  // --- presentation -------------------------------------------------------
  Column {
    id: chrome
    anchors.top: parent.top
    anchors.left: parent.left
    anchors.right: parent.right
    spacing: Style.space(8)

    Row {
      spacing: Style.space(8)

      Rectangle {
        id: saveButton
        width: saveLabel.width + Style.space(24)
        height: saveLabel.height + Style.space(12)
        radius: Style.cornerRadius
        color: Util.alpha(Color.accent, saveButton.activeFocus ? 0.35 : 0.18)
        border.color: Color.accent
        border.width: saveButton.activeFocus ? 2 : 1
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: "Save settings"
        Keys.onReturnPressed: settings.save()
        Keys.onSpacePressed: settings.save()
        Text {
          id: saveLabel
          anchors.centerIn: parent
          text: "Save"
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: settings.save() }
      }

      Rectangle {
        id: reloadButton
        width: reloadLabelText.width + Style.space(24)
        height: reloadLabelText.height + Style.space(12)
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, reloadButton.activeFocus ? 0.18 : 0.08)
        border.color: Util.alpha(Color.popups.text, 0.5)
        border.width: reloadButton.activeFocus ? 2 : 1
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: "Apply config file now"
        Keys.onReturnPressed: settings.reloadFromFile()
        Keys.onSpacePressed: settings.reloadFromFile()
        Text {
          id: reloadLabelText
          anchors.centerIn: parent
          text: "Apply now"
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: settings.reloadFromFile() }
      }
    }

    Text {
      visible: settings.banner !== ""
      text: settings.banner
      width: parent.width
      wrapMode: Text.Wrap
      font.family: Style.font.family
      font.bold: true
      font.pixelSize: Style.font.subtitle
      color: Color.popups.text
    }
    Text {
      visible: settings.bannerDetail !== ""
      text: settings.bannerDetail
      width: parent.width
      wrapMode: Text.Wrap
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.8)
    }
    Text {
      visible: settings.generalReadiness !== ""
      text: settings.generalReadiness
      width: parent.width
      wrapMode: Text.Wrap
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Color.popups.text
    }
  }

  Flickable {
    anchors.top: chrome.bottom
    anchors.topMargin: Style.space(12)
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.bottom: parent.bottom
    contentHeight: form.height
    clip: true

    Column {
      id: form
      width: parent.width
      spacing: Style.space(14)

      Repeater {
        model: settings.fields

        delegate: Column {
          id: fieldRow
          required property var modelData
          width: form.width
          spacing: Style.space(4)

          readonly property string fieldKey: String(modelData.key)
          readonly property string fieldType: String(modelData.type)
          readonly property var fieldEnum: modelData["enum"] || []

          Text {
            text: fieldRow.modelData.label + "  (" + fieldRow.fieldKey + " — "
              + settings.reloadLabel(String(fieldRow.modelData.reload)) + ")"
            width: parent.width
            wrapMode: Text.Wrap
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          // Toggle for booleans: state as text ("On"/"Off"), Space flips it.
          Rectangle {
            id: toggle
            visible: fieldRow.fieldType === "bool"
            width: toggleText.width + Style.space(24)
            height: toggleText.height + Style.space(10)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, toggle.activeFocus ? 0.18 : 0.08)
            border.color: Util.alpha(Color.popups.text, 0.5)
            border.width: toggle.activeFocus ? 2 : 1
            activeFocusOnTab: visible
            Accessible.role: Accessible.CheckBox
            Accessible.name: fieldRow.modelData.label

            readonly property bool current: {
              var v = settings.editedValue(fieldRow.fieldKey, fieldRow.modelData.value)
              return v === true || v === "true"
            }
            function flip() { settings.recordEdit(fieldRow.fieldKey, !current) }
            Keys.onSpacePressed: flip()
            Keys.onReturnPressed: flip()
            Text {
              id: toggleText
              anchors.centerIn: parent
              text: toggle.current ? "On" : "Off"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            MouseArea { anchors.fill: parent; onClicked: toggle.flip() }
          }

          // Cycle button for closed sets (tts.provider, log.level, ...):
          // arrows or Space step through the allowed values.
          Rectangle {
            id: cycler
            visible: fieldRow.fieldType !== "bool" && fieldRow.fieldEnum.length > 0
            width: cyclerText.width + Style.space(32)
            height: cyclerText.height + Style.space(10)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, cycler.activeFocus ? 0.18 : 0.08)
            border.color: Util.alpha(Color.popups.text, 0.5)
            border.width: cycler.activeFocus ? 2 : 1
            activeFocusOnTab: visible
            Accessible.role: Accessible.ComboBox
            Accessible.name: fieldRow.modelData.label

            readonly property string current:
              String(settings.editedValue(fieldRow.fieldKey, fieldRow.modelData.value))
            function step(delta) {
              var options = fieldRow.fieldEnum
              var at = options.indexOf(current)
              var next = ((at < 0 ? 0 : at) + delta + options.length) % options.length
              settings.recordEdit(fieldRow.fieldKey, String(options[next]))
            }
            Keys.onSpacePressed: step(1)
            Keys.onRightPressed: step(1)
            Keys.onLeftPressed: step(-1)
            Text {
              id: cyclerText
              anchors.centerIn: parent
              text: cycler.current + "  ↔"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            MouseArea { anchors.fill: parent; onClicked: cycler.step(1) }
          }

          // Text entry for everything else; lists are comma-separated (the
          // daemon accepts that form directly).
          Rectangle {
            id: entryBox
            visible: fieldRow.fieldType !== "bool" && fieldRow.fieldEnum.length === 0
            width: parent.width
            height: entry.height + Style.space(12)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, 0.06)
            border.color: entry.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
            border.width: entry.activeFocus ? 2 : 1

            TextInput {
              id: entry
              anchors.verticalCenter: parent.verticalCenter
              anchors.left: parent.left
              anchors.right: parent.right
              anchors.margins: Style.space(8)
              activeFocusOnTab: entryBox.visible
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
              clip: true
              Accessible.role: Accessible.EditableText
              Accessible.name: fieldRow.modelData.label

              // Initialise from the pending edit if there is one, else the
              // daemon's value; refreshes rebuild delegates so a Connections
              // hook is not needed.
              Component.onCompleted: {
                var v = settings.editedValue(fieldRow.fieldKey, fieldRow.modelData.value)
                if (Array.isArray(v)) v = v.join(",")
                text = v === null || v === undefined ? "" : String(v)
              }
              onTextEdited: settings.recordEdit(fieldRow.fieldKey, text)
            }
          }

          Text {
            visible: (settings.fieldErrors[fieldRow.fieldKey] || "") !== ""
            text: "Invalid: " + (settings.fieldErrors[fieldRow.fieldKey] || "")
            width: parent.width
            wrapMode: Text.Wrap
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          Text {
            visible: (settings.readiness[fieldRow.fieldKey] || "") !== ""
            text: settings.readiness[fieldRow.fieldKey] || ""
            width: parent.width
            wrapMode: Text.Wrap
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.8)
          }
        }
      }

      Text {
        visible: settings.secrets.length > 0
        text: "API keys (values are never shown or stored here)"
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }

      Repeater {
        model: settings.secrets
        delegate: Text {
          required property var modelData
          width: form.width
          wrapMode: Text.Wrap
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Util.alpha(Color.popups.text, 0.8)
          text: {
            var s = modelData
            if (s.env) {
              return s.env + ": " + (s.env_set ? "set" : "not set") + "  (" + s.endpoint + ")"
            }
            if (s.inline_key) {
              return s.endpoint + ": inline key in config.toml (prefer api_key_env)"
            }
            return s.endpoint + ": no key configured (fine for local endpoints)"
          }
        }
      }
    }
  }
}
