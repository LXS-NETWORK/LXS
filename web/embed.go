// Package web ships the launchpad UI embedded in the binary, so any node can serve it
// (lxs node -launchpad). Bundling the frontend into the same binary the miners run means
// the "easy" way to create and trade coins survives as long as the chain does — it does
// not depend on the operator hosting a website.
package web

import "embed"

//go:embed launchpad
var Launchpad embed.FS

// Dashboard is the mining dashboard UI, served by a node with -mine-dashboard so a miner
// sees balance / blocks won / hashrate / peers in the browser.
//
//go:embed dashboard
var Dashboard embed.FS
