import { defineConfig } from "vite";

// The bundle is embedded into the Go binary and served by the Wails asset
// server from the root, so asset URLs must be relative — an absolute /assets
// path breaks as soon as the window loads anything but "/".
//
// No Wails vite plugin: this frontend calls bound methods by name through
// @wailsio/runtime (see src/bridge.ts) instead of generated bindings, which
// keeps the build a plain Vite build and the toolchain one step shorter
// (docs/canonical.md §7 item 3).
export default defineConfig({
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "es2022",
  },
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
});
