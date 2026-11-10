import React from 'react';
import { createRoot } from 'react-dom/client';
import App from './App.jsx';
import { GatewayProvider } from './lib/GatewayContext.jsx';
import './styles/font.css';
import './styles/base.css';
import './styles/extra.css';
import './styles/loader.css';
import './components/Radar.css';
import './components/BlobCursor.css';

createRoot(document.getElementById('root')).render(
  <GatewayProvider>
    <App />
  </GatewayProvider>,
);
