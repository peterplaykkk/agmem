import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Served from the agmem single binary via go:embed (web/embed.go), mounted
// at the domain root. Dev server proxies /api to a local agmem instance.
export default defineConfig({
  base: "/",
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    proxy: {
      "/api": "http://127.0.0.1:8080",
    },
  },
});
