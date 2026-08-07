// Editor build.
//
// esbuild directly rather than a bundler with a config format of its own. The
// output is embedded into the Go binary, so the only requirements are: one JS
// file, one CSS file, and no runtime fetches — the CSP on the dashboard's proxy
// blocks external hosts, and an edge box has no internet anyway.

import * as esbuild from 'esbuild';
import { copyFileSync, mkdirSync } from 'node:fs';

const watch = process.argv.includes('--watch');

mkdirSync('dist', { recursive: true });
copyFileSync('src/index.html', 'dist/index.html');

/** @type {import('esbuild').BuildOptions} */
const options = {
  entryPoints: ['src/main.ts'],
  outdir: 'dist',
  bundle: true,
  format: 'esm',
  target: 'es2022',
  minify: !watch,
  sourcemap: watch ? 'inline' : false,
  // Everything inlined. No CDN, no external stylesheet.
  loader: { '.svg': 'text' },
  logLevel: 'info',
};

if (watch) {
  const ctx = await esbuild.context(options);
  await ctx.watch();
  console.log('watching');
} else {
  await esbuild.build(options);
}
