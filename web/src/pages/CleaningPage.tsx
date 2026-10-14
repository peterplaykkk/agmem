// 清洗中视图（裁决 6）：按 kb 聚合 + task 明细展开，URL ?kb=&status= 深链复现。
// kb 输入本地 state + 300ms 防抖/回车提交 → replace 写 URL，不逐键产生历史项。
import { useEffect, useRef, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Stack from '@mui/material/Stack';
import TablePagination from '@mui/material/TablePagination';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { api, TASK_STATUSES } from '../api';
import { StatusChip, normalizeStatus } from '../components/StatusChip';
import { EmptyState, ErrorState, LoadingState } from '../components/states';
import { useApi } from '../components/useApi';
import { useDocumentTitle } from '../components/useDocumentTitle';

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100];

function summarize(text: string, len = 160): string {
  const s = text.replace(/\s+/g, ' ').trim();
  return s.length > len ? `${s.slice(0, len)}…` : s;
}

export function CleaningPage() {
  const { t } = useTranslation();
  const [params, setParams] = useSearchParams();
  const kb = (params.get('kb') ?? '').trim();
  const status = normalizeStatus(params.get('status') ?? 'cleaning');
  const page = Math.max(1, Number.parseInt(params.get('page') ?? '1', 10) || 1);
  const rawPageSize = Number.parseInt(params.get('page_size') ?? '20', 10);
  const pageSize = (PAGE_SIZE_OPTIONS as number[]).includes(rawPageSize) ? rawPageSize : 20;
  useDocumentTitle(t('docTitle.cleaning'));

  const [kbInput, setKbInput] = useState(kb);
  const debounceRef = useRef<number | undefined>(undefined);
  useEffect(() => setKbInput(kb), [kb]);

  // 分页参数规范化（replace）：非法 page_size 回写；越界页回末页。
  useEffect(() => {
    const p = new URLSearchParams(params);
    let changed = false;
    if (p.get('page_size') !== null && !PAGE_SIZE_OPTIONS.includes(rawPageSize)) {
      p.set('page_size', String(pageSize));
      changed = true;
    }
    if (changed) setParams(p, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params]);

  const points = useApi(
    (signal) => api.points(kb, status, page, pageSize, signal),
    [kb, status, page, pageSize],
    { pollMs: status === 'cleaning' ? 5000 : 0 },
  );
  const total = points.data?.total;
  const maxPage = total != null ? Math.max(1, Math.ceil(total / pageSize)) : 1;
  useEffect(() => {
    if (total != null && page > maxPage) setPageParam(maxPage, pageSize, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total, maxPage, page, pageSize]);

  function setParam(key: 'kb' | 'status', value: string) {
    const p = new URLSearchParams(params);
    p.delete('page');
    if (value === '' || (key === 'status' && value === 'cleaning')) p.delete(key);
    else p.set(key, value);
    setParams(p);
  }
  function setPageParam(nextPage: number, nextSize: number, replace: boolean) {
    const p = new URLSearchParams(params);
    if (nextPage <= 1) p.delete('page');
    else p.set('page', String(nextPage));
    if (nextSize === 20) p.delete('page_size');
    else p.set('page_size', String(nextSize));
    setParams(p, { replace });
  }
  function commitKb(value: string) {
    const v = value.trim();
    if (v === kb) return;
    const p = new URLSearchParams(params);
    p.delete('page');
    if (v === '') p.delete('kb');
    else p.set('kb', v);
    setParams(p, { replace: true });
  }
  function onKbInput(v: string) {
    setKbInput(v);
    window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => commitKb(v), 300);
  }

  const items = points.data?.items ?? [];
  const groups = new Map<number, typeof items>();
  for (const it of items) {
    const list = groups.get(it.TaskID) ?? [];
    list.push(it);
    groups.set(it.TaskID, list);
  }
  const legacy = total == null;

  return (
    <Stack spacing={1.5}>
      <Typography variant="h1">{t('cleaning.title')}</Typography>
      <Stack
        component="form"
        direction={{ xs: 'column', sm: 'row' }}
        spacing={1}
        aria-label={t('cleaning.title')}
        onSubmit={(e) => {
          e.preventDefault();
          window.clearTimeout(debounceRef.current);
          commitKb(kbInput);
        }}
      >
        <TextField
          label={t('cleaning.kbLabel')}
          value={kbInput}
          onChange={(e) => onKbInput(e.target.value)}
          onBlur={() => {
            window.clearTimeout(debounceRef.current);
            commitKb(kbInput);
          }}
          sx={{ maxWidth: 260 }}
          placeholder="sre"
        />
        <FormControl sx={{ minWidth: 180 }}>
          <InputLabel id="cleaning-status-label">{t('cleaning.statusLabel')}</InputLabel>
          <Select
            labelId="cleaning-status-label"
            label={t('cleaning.statusLabel')}
            value={status}
            onChange={(e) => setParam('status', e.target.value)}
          >
            {TASK_STATUSES.map((s) => (
              <MenuItem key={s} value={s}>{t(`status.${s}`)}</MenuItem>
            ))}
          </Select>
        </FormControl>
        <Button variant="outlined" onClick={points.retry} disabled={points.loading || points.reloading} sx={{ alignSelf: 'center' }}>
          {t('common.refresh')}
        </Button>
        {status === 'cleaning' && kb !== '' && (
          <Typography variant="body2" color="text.secondary" sx={{ alignSelf: 'center' }}>
            {t('cleaning.autoRefresh')}
          </Typography>
        )}
      </Stack>

      {kb === '' ? (
        <EmptyState message={t('cleaning.needKb')} />
      ) : points.loading ? (
        <LoadingState />
      ) : points.error != null ? (
        <ErrorState error={points.error} onRetry={points.retry} />
      ) : items.length === 0 ? (
        <EmptyState message={t('cleaning.empty')} />
      ) : (
        <>
          {!legacy && (
            <Typography variant="body2" color="text.secondary">
              {t('common.count', { count: total ?? items.length })}
            </Typography>
          )}
          {legacy && (
            <Typography variant="body2" color="warning.main" role="note">
              {t('items.legacyOnly', { n: items.length })}
            </Typography>
          )}
          <Stack spacing={1.5}>
            {[...groups.entries()].map(([taskID, group]) => (
              <Box key={taskID} sx={{ border: 1, borderColor: 'divider', borderRadius: 1, bgcolor: 'background.paper' }}>
                <Stack
                  direction="row"
                  spacing={1.5}
                  alignItems="center"
                  sx={{ px: 1.5, py: 1, borderBottom: 1, borderColor: 'divider' }}
                >
                  <Typography variant="h2">
                    {t('cleaning.groupTitle', { task: taskID, count: group.length })}
                  </Typography>
                  <Box sx={{ flex: 1 }} />
                  <StatusChip status={group[0].Status} />
                </Stack>
                <Stack divider={<Box sx={{ borderTop: 1, borderColor: 'divider' }} />} component="ul" sx={{ m: 0, p: 0, listStyle: 'none' }}>
                  {group.map((it) => (
                    <Box key={it.ID} component="li" sx={{ px: 1.5, py: 1 }}>
                      <Stack direction="row" spacing={1.5} alignItems="baseline">
                        <Typography variant="body2" fontWeight={600}>#{it.ID}</Typography>
                        <StatusChip status={it.Status} />
                        <Typography variant="body2" sx={{ color: 'text.secondary', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                          {summarize(it.OriginalText)}
                        </Typography>
                      </Stack>
                      {it.Error?.Valid && it.Error.String !== '' && (
                        <Typography variant="body2" color="error" sx={{ mt: 0.5 }}>
                          {t('common.failure')}：{it.Error.String}
                        </Typography>
                      )}
                    </Box>
                  ))}
                </Stack>
              </Box>
            ))}
          </Stack>
          <TablePagination
            component="div"
            count={total ?? items.length}
            page={page - 1}
            onPageChange={(_, p) => setPageParam(p + 1, pageSize, false)}
            rowsPerPage={pageSize}
            onRowsPerPageChange={(e) => setPageParam(1, Number.parseInt(e.target.value, 10), false)}
            rowsPerPageOptions={PAGE_SIZE_OPTIONS}
            labelRowsPerPage={t('pagination.rowsPerPage')}
            labelDisplayedRows={({ from, to, count }) =>
              t('pagination.range', { from, to, count: legacy ? items.length : count })
            }
            getItemAriaLabel={(type) =>
              type === 'previous' ? t('pagination.prev') : type === 'next' ? t('pagination.next') : ''
            }
          />
        </>
      )}
    </Stack>
  );
}
