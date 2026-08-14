import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@xyflow/react/dist/style.css';
import './styles.css';
import { QuickstartApp } from './QuickstartApp';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QuickstartApp />
  </StrictMode>
);
