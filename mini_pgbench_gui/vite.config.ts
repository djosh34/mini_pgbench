import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  // For pnpm dev: send /api to a benchmark on localhost:8080, as main.go does.
  server: {
    proxy: {
      "/api": { target: "http://localhost:8080", rewrite: (path) => path.replace(/^\/api/, "") },
    },
  },
});
