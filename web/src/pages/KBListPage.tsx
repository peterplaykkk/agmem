import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Card from '@mui/material/Card';
import CardActionArea from '@mui/material/CardActionArea';
import CardContent from '@mui/material/CardContent';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink, useNavigate } from 'react-router-dom';
import { api, ApiError, isNotReady } from '../api';
import { ErrorState, LoadingState } from '../components/states';
import { useApi } from '../components/useApi';
import { useDocumentTitle } from '../components/useDocumentTitle';

export function KBListPage() {
  const { t } = useTranslation();
  useDocumentTitle(t('docTitle.kbs'));
  const navigate = useNavigate();
  const [openName, setOpenName] = useState('');
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState('');
  const [nameErr, setNameErr] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [createErr, setCreateErr] = useState<ApiError | null>(null);

  // GET /api/kb 后端骨架未提供 → 列表区渲染「后端接口未就绪」降级态，
  // 并提供按名称打开 + 新建知识库（两者端点已实现）。
  const kbs = useApi((signal) => api.listKBs(signal), [], { pollMs: 0 });

  async function submitCreate(e: FormEvent) {
    e.preventDefault();
    const v = name.trim();
    if (v === '') {
      setNameErr(t('kbList.nameRequired'));
      return;
    }
    setNameErr(null);
    setSubmitting(true);
    setCreateErr(null);
    try {
      await api.createKB(v);
      setCreateOpen(false);
      setName('');
      navigate(`/kb/${encodeURIComponent(v)}`);
    } catch (err) {
      setCreateErr(err instanceof ApiError ? err : new ApiError(0, String(err)));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" alignItems="center" spacing={1.5}>
        <Typography variant="h1">{t('kbList.title')}</Typography>
        <Box sx={{ flex: 1 }} />
        <Button variant="contained" onClick={() => setCreateOpen(true)}>
          {t('kbList.create')}
        </Button>
      </Stack>
      <Typography variant="body2" color="text.secondary">
        {t('kbList.description')}
      </Typography>

      {kbs.loading ? (
        <LoadingState />
      ) : kbs.error != null ? (
        <Stack spacing={2}>
          <ErrorState error={kbs.error} onRetry={kbs.retry} />
          <OpenByName value={openName} onChange={setOpenName} onSubmit={() => {
            const v = openName.trim();
            if (v !== '') navigate(`/kb/${encodeURIComponent(v)}`);
          }} />
        </Stack>
      ) : (
        <>
          {(kbs.data?.kbs ?? []).length === 0 ? (
            <OpenByName value={openName} onChange={setOpenName} onSubmit={() => {
              const v = openName.trim();
              if (v !== '') navigate(`/kb/${encodeURIComponent(v)}`);
            }} />
          ) : (
            <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: 'repeat(auto-fill, minmax(240px, 1fr))' }, gap: 1.5 }}>
              {(kbs.data?.kbs ?? []).map((kb) => (
                <Card key={kb.name} sx={{ maxHeight: 150 }}>
                  <CardActionArea component={RouterLink} to={`/kb/${encodeURIComponent(kb.name)}`}>
                    <CardContent>
                      <Typography variant="h2">{kb.name}</Typography>
                      <Typography variant="body2" color="text.secondary">
                        {kb.owner}
                      </Typography>
                    </CardContent>
                  </CardActionArea>
                </Card>
              ))}
            </Box>
          )}
        </>
      )}

      <Dialog open={createOpen} onClose={() => setCreateOpen(false)} aria-labelledby="create-kb-title" maxWidth="sm" fullWidth>
        <DialogTitle id="create-kb-title" component="h2">{t('kbList.createTitle')}</DialogTitle>
        <form onSubmit={submitCreate} noValidate>
          <DialogContent>
            <Stack spacing={1.5}>
              <TextField
                label={t('kbList.nameLabel')}
                value={name}
                onChange={(e) => setName(e.target.value)}
                error={nameErr != null}
                helperText={nameErr ?? t('kbList.oneLevelHint')}
                aria-invalid={nameErr != null}
                required
                autoFocus
                fullWidth
              />
              {createErr != null && (
                <Typography variant="body2" color="error" role="alert">
                  {isNotReady(createErr) ? t('common.notReady') : `${t('common.errorPrefix')}：${createErr.message}`}
                </Typography>
              )}
            </Stack>
          </DialogContent>
          <DialogActions>
            <Button onClick={() => setCreateOpen(false)}>{t('common.cancel')}</Button>
            <Button type="submit" variant="contained" disabled={submitting}>
              {t('common.create')}
            </Button>
          </DialogActions>
        </form>
      </Dialog>
    </Stack>
  );
}

function OpenByName({ value, onChange, onSubmit }: { value: string; onChange: (v: string) => void; onSubmit: () => void }) {
  const { t } = useTranslation();
  return (
    <Stack component="form" direction={{ xs: 'column', sm: 'row' }} spacing={1} onSubmit={(e) => { e.preventDefault(); onSubmit(); }}>
      <TextField
        label={t('kbList.openManually')}
        placeholder={t('kbList.openPlaceholder')}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        sx={{ maxWidth: 320 }}
      />
      <Button type="submit" variant="outlined" sx={{ alignSelf: 'center' }}>
        {t('kbList.openAction')}
      </Button>
    </Stack>
  );
}
