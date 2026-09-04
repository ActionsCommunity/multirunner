// @ts-check
import { defineConfig } from "astro/config";

// https://docs.astro.build/en/guides/deploy/github/
// The project is published below /multirunner/, so every internal URL must use
// Astro's generated base path instead of assuming the domain root.
export default defineConfig({
  site: "https://actionscommunity.github.io",
  base: "/multirunner/",
});
