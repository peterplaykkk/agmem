import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Route, Routes } from 'react-router-dom';
import { AppShell } from './components/AppShell';
import './i18n';
import { CleaningPage } from './pages/CleaningPage';
import { KBDetailPage, NotFoundPage } from './pages/KBDetailPage';
import { KBListPage } from './pages/KBListPage';
import { LoginPage } from './pages/LoginPage';
import { theme } from './theme';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<KBListPage />} />
          <Route path="login" element={<LoginPage />} />
          <Route path="kb/:kb" element={<KBDetailPage />} />
          <Route path="cleaning" element={<CleaningPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </ThemeProvider>
  </StrictMode>,
);
