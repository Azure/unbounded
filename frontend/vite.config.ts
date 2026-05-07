// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const proxyTarget = env.VITE_CONTROLLER_URL || 'http://localhost:8080';

  return {
    plugins: [react()],
    // Keep Vite cache in a stable location to speed incremental rebuilds.
    cacheDir: '.vite',
    base: '/static/',
    build: {
      // Modern browsers only.
      target: 'esnext',
      outDir: 'dist',
      emptyOutDir: true,
      // Production sourcemaps add build overhead and are not required here.
      sourcemap: false,
      // Keep minification on the fastest available path.
      minify: 'esbuild',
      // Skip compressed size computation during build for faster completion.
      reportCompressedSize: false,
      rollupOptions: {
        output: {
          manualChunks: {
            tanstack: ['@tanstack/react-table', '@tanstack/table-core'],
            reagraph: ['reagraph'],
            three: ['three']
          }
        }
      }
    },
    server: {
      proxy: {
        '/status/json': {
          target: proxyTarget,
          changeOrigin: true
        },
        '/status/ws': {
          target: proxyTarget,
          ws: true,
          changeOrigin: true
        }
      }
    }
  };
});
