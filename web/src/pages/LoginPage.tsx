import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Paper from '@mui/material/Paper';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useDocumentTitle } from '../components/useDocumentTitle';

export function LoginPage() {
  const { t } = useTranslation();
  useDocumentTitle(t('docTitle.login'));
  const [connecting, setConnecting] = useState(false);
  const [notReady, setNotReady] = useState(false);

  async function startLogin() {
    setConnecting(true);
    setNotReady(false);
    try {
      // redirect:'manual'：OIDC 已配置时返回 opaqueredirect（视为成功，整体跳转）；
      // 未配置时后端返回 501/503 JSON → 降级提示。
      const res = await fetch('/api/login', { redirect: 'manual', credentials: 'same-origin' });
      if (res.type === 'opaqueredirect' || res.ok) {
        window.location.href = '/api/login';
        return;
      }
      setNotReady(true);
    } catch {
      setNotReady(true);
    } finally {
      setConnecting(false);
    }
  }

  return (
    <Stack alignItems="center" sx={{ mt: 8 }}>
      <Paper sx={{ p: 3, width: '100%', maxWidth: 380 }} component="section" aria-labelledby="login-h1">
        <Stack spacing={2}>
          <Typography id="login-h1" variant="h1">
            {t('login.title')}
          </Typography>
          {notReady && <Alert severity="info">{t('login.notReady')}</Alert>}
          <Button
            variant="contained"
            onClick={startLogin}
            disabled={connecting}
            sx={{ py: 1 }}
          >
            {connecting ? t('login.connecting') : t('login.feishu')}
          </Button>
          <Typography variant="body2" color="text.secondary">
            {t('login.hint')}
          </Typography>
        </Stack>
      </Paper>
    </Stack>
  );
}
