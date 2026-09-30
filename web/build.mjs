import { build } from 'esbuild';
import { mkdir, cp } from 'node:fs/promises';
await mkdir('dist', { recursive: true });
await cp('public', 'dist', { recursive: true });
await build({ entryPoints: ['src/main.ts'], bundle: true, outdir: 'dist/assets', entryNames: 'app', minify: true, target: ['es2022'], sourcemap: false, legalComments: 'none' });
