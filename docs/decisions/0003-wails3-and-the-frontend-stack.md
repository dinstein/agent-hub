# 0003 — Wails3 alpha, vanilla TypeScript, and the three-file blast radius

> **Status** active · **Behaviour** [subsystems/gui.md](../subsystems/gui.md)

`wails/v3` alpha plus vanilla TypeScript and Vite, with `@wailsio/runtime` as the only frontend runtime
dependency.

**The fallback plan is not "switch frameworks", it is compressing the alpha dependency down to three
files.** Only `gui_main.go`, `services/service_wails.go` and `tray_wails.go` sit behind `//go:build
wails`; the bodies they assemble carry no tag and so compile, vet and unit-test in CI without graphics
libraries. The day the alpha stops building, only those three files break.

The frontend also skips generated bindings in favour of `Call.ByName` plus `Events.On`.
