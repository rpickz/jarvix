import QtQuick
import qs.Commons
import qs.Ui

// The standard empty-state sentence for a collection tab (issue #91): what
// would appear here and how to make that happen. One shape, shared by every
// tab (and by the sibling management tickets), so "nothing yet" always reads
// the same way. Callers centre it in the tab and set `text`.
Text {
  horizontalAlignment: Text.AlignHCenter
  wrapMode: Text.Wrap
  font.family: Style.font.family
  font.pixelSize: Style.font.subtitle
  color: Util.alpha(Color.popups.text, 0.7)
}
