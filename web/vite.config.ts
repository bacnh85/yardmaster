import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  base: "/",
  test: {
    // Chart.test needs a DOM (effects + observers); the rest don't mind
    environment: "jsdom",
  },
  build: { outDir: "dist", emptyOutDir: true },
  server: {
    proxy: {
      "/admin": process.env.YM_DEV_TARGET || "http://localhost:8787",
      "/v1": process.env.YM_DEV_TARGET || "http://localhost:8787",
    },
  },
});
