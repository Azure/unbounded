import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  base: '/architecture-tool/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: false,
    target: 'esnext'
  }
});
