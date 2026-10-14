import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink } from 'react-router-dom';
import { ApiError, isNotReady } from '../api';
/** 五态：loading / empty / error(+retry) / notReady(501/503 降级) 由各页面按此渲染。 */
export function LoadingState() {
  const { t } = useTranslation();
  return (
    <Stack alignItems="center" sx={{ py: 6 }} role="status" aria-live="polite">
      <CircularProgress size={28} aria-label={t('common.loading')} />
      <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
        {t('common.loading')}
      </Typography>
    </Stack>
  );
}

export function NotReadyState() {
  const { t } = useTranslation();
  return (
    <Alert severity="info" sx={{ my: 2 }}>
      <Typography variant="body2" fontWeight={600}>
        {t('common.notReady')}
      </Typography>
      <Typography variant="body2" color="text.secondary">
        {t('common.notReadyDetail')}
      </Typography>
    </Alert>
  );
}

export function ErrorState({ error, onRetry }: { error: ApiError; onRetry: () => void }) {
  const { t } = useTranslation();
  if (isNotReady(error)) return <NotReadyState />;
  if (error.status === 401) {
    return (
      <Alert
        severity="warning"
        sx={{ my: 2 }}
        action={
          <Button component={RouterLink} to="/login" size="small">
            {t('common.goLogin')}
          </Button>
        }
      >
        {t('common.unauthorized')}
      </Alert>
    );
  }
  return (
    <Alert
      severity="error"
      sx={{ my: 2 }}
      action={
        <Button color="inherit" size="small" onClick={onRetry}>
          {t('common.retry')}
        </Button>
      }
    >
      {t('common.errorPrefix')}：{error.message}
    </Alert>
  );
}

export function EmptyState({ message, hint }: { message: string; hint?: string }) {
  return (
    <Box sx={{ py: 5, textAlign: 'center' }} role="status">
      <Typography variant="body2" color="text.secondary">
        {message}
      </Typography>
      {hint != null && (
        <Typography variant="body2" color="text.disabled" sx={{ mt: 0.5 }}>
          {hint}
        </Typography>
      )}
    </Box>
  );
}
