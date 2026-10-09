// Package docs holds the documents the server shows in the UI.
package docs

import _ "embed"

// PlatformArchitecture is the design of the whole multi-site platform, as a
// self-contained HTML page: no scripts and nothing loaded from the network.
//
//go:embed platform-architecture.html
var PlatformArchitecture []byte
