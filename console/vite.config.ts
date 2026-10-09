/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'node:path';

// Built into internal/console/dist, which embed.FS picks up so that one binary
// includes the UI (R-253).
// Where `npm run dev` proxies the API to. 8080 is the Compose default; set
// PANDO_DEV_API when the local install is somewhere else.
const devServer = process.env.PANDO_DEV_API || 'http://localhost:8080';

export default defineConfig({
  plugins: [react()],

  // Assets live under Pando's one reserved path, on every hostname.
  //
  // The console's HTML is served at "/", at "/admin/…" and — for someone
  // signing in to reach an app on its own hostname — at "/.pando/login". Its
  // assets have to resolve from all three. Relative URLs break on the deep
  // admin routes; "/assets/…" breaks on an app's hostname, where the top of
  // the domain is the app and not Pando, which is R-167's failure happening to
  // our own console. An absolute path under the one prefix no app can claim
  // works everywhere, and is why that prefix is reserved on every hostname
  // rather than only on Pando's own (see httpapi.ReservedPrefix).
  base: '/.pando/',

  resolve: {
    alias: {
      // The design system's barrel. Components are plain .jsx with sibling
      // .d.ts files; TypeScript reads the declarations, Vite compiles the JSX.
      '@design': resolve(__dirname, 'src/design/index.js'),
      '@api': resolve(__dirname, 'src/api'),
    },
  },

  build: {
    outDir: resolve(__dirname, '../internal/console/dist'),

    // scripts/clean-dist.mjs does the emptying, in prebuild. Vite's own
    // emptyOutDir takes everything, and one file in there is committed:
    // dist/README.md, which is what `//go:embed all:dist` matches on a fresh
    // clone where no console has been built. Letting Vite delete it meant the
    // next `git add -A` staged its removal and the repository went back to not
    // compiling.
    emptyOutDir: false,

    // No source maps in the shipped binary: they would double its size and the
    // console is not debugged in production.
    sourcemap: false,
  },

  // `npm run coverage`, for SonarCloud. Paths in the report are relative to
  // the repository root (console/src/…), which is where the scanner runs.
  test: {
    coverage: {
      provider: 'v8',
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/design/**', 'src/api/types.gen.ts', 'src/**/*.test.{ts,tsx}', 'src/**/*.d.ts'],
      reporter: [['lcov', { projectRoot: '..' }]],
      reportsDirectory: 'coverage',
    },
  },

  server: {
    // `npm run dev` proxies to a locally running Pando so the console can be
    // developed against a real API rather than fixtures.
    proxy: {
      // 8080, which is what `docker compose up` gives you. An install on a
      // different port is a PANDO_PORT away, and so is this: set PANDO_DEV_API.
      '/api': { target: devServer, changeOrigin: true },
      '/.well-known': { target: devServer, changeOrigin: true },
    },
  },
});
