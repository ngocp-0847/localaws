import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// The console is served by localaws itself at /_localaws/ and talks to the same origin, so in
// development every API path is proxied to a running emulator (LOCALAWS_URL, default :4566).
const target = process.env.LOCALAWS_URL ?? "http://localhost:4566";

export default defineConfig({
  base: "/_localaws/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "src") } },
  build: { outDir: "dist", emptyOutDir: true, chunkSizeWarningLimit: 900 },
  server: {
    port: 5173,
    proxy: {
      "^/(?!_localaws/).*": { target, changeOrigin: true }, // every AWS API (JSON targets, S3 paths)
      "/_localaws/api": { target, changeOrigin: true },
      "/_localaws/reset": { target, changeOrigin: true },
      "/_localaws/health": { target, changeOrigin: true },
    },
  },
});
