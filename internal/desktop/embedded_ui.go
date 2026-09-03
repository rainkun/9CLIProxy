package desktop

import _ "embed"

// embeddedManagementHTML is the complete single-file management UI.
// Keeping it in the desktop package makes the UI self-contained in the
// production executable; no adjacent static files are required.
//
//go:embed management.html
var embeddedManagementHTML []byte
