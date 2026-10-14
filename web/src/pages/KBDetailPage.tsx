import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControl from '@mui/material/FormControl';import InputLabel from '@mui/material/InputLabel';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Stack from '@mui/material/Stack';
import Tab from '@mui/material/Tab';

import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TablePagination from '@mui/material/TablePagination';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useMemo, useState, useEffect, type FormEvent, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Link as RouterLink,
  useLocation,
  useParams,
  useSearchParams,
} from 'react-router-dom';
import {
  api,
  ApiError,
  isNotReady,
  TASK_STATUSES,
  type TaskItem,
} from '../api';
import { StatusChip, normalizeStatus } from '../components/StatusChip';
import { EmptyState, ErrorState, LoadingState } from '../components/states';
import { useApi } from '../components/useApi';
import { useDocumentTitle } from '../components/useDocumentTitle';

const TABS = ['dirs', 'items'] as const;
type TabKey = (typeof TABS)[number];
const PAGE_SIZE_OPTIONS = [10, 20, 50, 100];

export function KBDetailPage() {
  const { t } = useTranslation();
  const { kb = '' } = useParams();
  const [params, setParams] = useSearchParams();
  useDocumentTitle(t('docTitle.kbDetail', { kb }));

  const rawTab = params.get('tab') ?? 'dirs';
  const tab: TabKey = (TABS as readonly string[]).includes(rawTab) ? (rawTab as TabKey) : 'dirs';
  const rawStatus = params.get('status') ?? '';
  const status = rawStatus === '' ? 'cleaning' : normalizeStatus(rawStatus);
  const page = Math.max(1, Number.parseInt(params.get('page') ?? '1', 10) || 1);
  const rawPageSize = Number.parseInt(params.get('page_size') ?? '20', 10);
  const pageSize = (PAGE_SIZE_OPTIONS as number[]).includes(rawPageSize) ? rawPageSize : 20;

  // 非法 tab/status/page_size 用 replace 回写规范值（URL 始终保持规范形式）。
  useEffect(() => {
    const p = new URLSearchParams(params);
    let changed = false;
    if (tab === 'dirs') {
      if (params.get('tab') !== null) { p.delete('tab'); changed = true; }
    } else if (params.get('tab') !== tab) {
      p.set('tab', tab);
      changed = true;
    }
    if (status === 'cleaning') {
      if (params.get('status') !== null) { p.delete('status'); changed = true; }
    } else if (params.get('status') !== status) {
      p.set('status', status);
      changed = true;
    }
    if (params.get('page_size') !== null && !PAGE_SIZE_OPTIONS.includes(rawPageSize)) {
      p.set('page_size', String(pageSize));
      changed = true;
    }
    if (changed) setParams(p, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params]);

  function setTab(next: string) {
    const p = new URLSearchParams(params);
    p.delete('page');
    if (next === 'dirs') p.delete('tab');
    else p.set('tab', next);
    setParams(p, { replace: false });
  }
  function setStatus(next: string) {
    const p = new URLSearchParams(params);
    p.delete('page');
    if (next === 'cleaning') p.delete('status');
    else p.set('status', next);
    p.set('tab', 'items');
    setParams(p);
  }
  function setPage(nextPage: number, nextSize: number) {
    const p = new URLSearchParams(params);
    if (nextPage <= 1) p.delete('page');
    else p.set('page', String(nextPage));
    if (nextSize === 20) p.delete('page_size');
    else p.set('page_size', String(nextSize));
    setParams(p);
  }

  const version = useApi((signal) => api.version(kb, signal), [kb]);
  const [uploadOpen, setUploadOpen] = useState(false);

  return (
    <Stack spacing={1.5}>
      <Stack direction="row" alignItems="baseline" spacing={1.5} flexWrap="wrap">
        <Typography variant="h1">{kb}</Typography>
        <Typography variant="body2" color="text.secondary">
          {t('kbDetail.title')}
          {version.data != null && <> · {t('kbDetail.versionLabel', { version: version.data.version })}</>}
        </Typography>
        <Box sx={{ flex: 1 }} />
        <Button variant="contained" onClick={() => setUploadOpen(true)}>
          {t('kbDetail.upload')}
        </Button>
      </Stack>
      {version.error != null && !isNotReady(version.error) && (
        <Alert severity="warning" sx={{ py: 0.5 }}>{t('kbDetail.versionLoadFailed')}</Alert>
      )}

      <Stack direction="row" alignItems="center" spacing={0.5}>
        <Tabs value={tab} onChange={(_, v: string) => setTab(v)} aria-label={t('kbDetail.title')}>
          <Tab value="dirs" label={t('kbDetail.tab.dirs')} component={RouterLink} to={`/kb/${encodeURIComponent(kb)}`} />
          <Tab value="items" label={t('kbDetail.tab.items')} component={RouterLink} to={`/kb/${encodeURIComponent(kb)}?tab=items`} />
        </Tabs>
        {/* 清洗中视图是独立路由（/cleaning?kb=），以带图标的跳转链接呈现并带 aria 说明，不与页内 tab 混淆选中态。 */}
        <Button
          component={RouterLink}
          to={`/cleaning?kb=${encodeURIComponent(kb)}`}
          size="small"
          sx={{ textTransform: 'none', color: 'text.secondary', px: 1.5 }}
          aria-label={`${t('kbDetail.tab.cleaning')}（${t('nav.cleaning')}）`}
        >
          {t('kbDetail.tab.cleaning')}
          <svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" style={{ marginLeft: 4 }}>
            <path d="M14 3h7v7h-2V6.4l-9.3 9.3-1.4-1.4L17.6 5H14V3zM5 5h6v2H7v10h10v-4h2v6H5V5z" fill="currentColor" />
          </svg>
        </Button>
      </Stack>

      {tab === 'dirs' ? (
        <DirsPanel kb={kb} />
      ) : (
        <ItemsPanel
          kb={kb}
          status={status}
          page={page}
          pageSize={pageSize}
          onStatus={setStatus}
          onPage={setPage}
        />
      )}

      <UploadDialog open={uploadOpen} kb={kb} onClose={() => setUploadOpen(false)} />
    </Stack>
  );
}

// —— 目录 ——

function DirsPanel({ kb }: { kb: string }) {
  const { t } = useTranslation();
  const dirs = useApi((signal) => api.listDirs(kb, signal), [kb]);
  const [createOpen, setCreateOpen] = useState(false);
  const [created, setCreated] = useState<string | null>(null);

  return (
    <Stack spacing={1.5}>
      <Stack direction="row" spacing={1}>
        <Button variant="outlined" onClick={() => setCreateOpen(true)}>
          {t('dirs.create')}
        </Button>
      </Stack>
      {created != null && (
        <Alert severity="success" onClose={() => setCreated(null)}>
          {t('dirs.created')} {created}
        </Alert>
      )}
      {dirs.loading ? (
        <LoadingState />
      ) : dirs.error != null ? (
        <Stack spacing={1}>
          <ErrorState error={dirs.error} onRetry={dirs.retry} />
          {isNotReady(dirs.error) && (
            <Typography variant="body2" color="text.secondary">
              {t('dirs.listNotReady')}
            </Typography>
          )}
        </Stack>
      ) : (dirs.data?.dirs ?? []).length === 0 ? (
        <EmptyState message={t('common.empty')} />
      ) : (
        <TableContainer>
          <Table size="small" aria-label={t('kbDetail.tab.dirs')}>
            <TableHead>
              <TableRow>
                <TableCell>{t('dirs.nameLabel')}</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {(dirs.data?.dirs ?? []).map((d) => (
                <TableRow key={d.name}>
                  <TableCell>{d.name}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}
      <CreateDirDialog open={createOpen} kb={kb} onClose={() => setCreateOpen(false)} onCreated={(n) => { setCreated(n); dirs.retry(); }} />
    </Stack>
  );
}

function CreateDirDialog({ open, kb, onClose, onCreated }: { open: boolean; kb: string; onClose: () => void; onCreated: (name: string) => void }) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const v = name.trim();
    if (v === '') { setErr(t('dirs.nameRequired')); return; }
    if (v.includes('/')) { setErr(t('dirs.nameNoSlash')); return; }
    setErr(null);
    setSubmitting(true);
    try {
      await api.createDir(kb, v);
      setName('');
      onCreated(v);
      onClose();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.message : String(ex));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onClose={onClose} aria-labelledby="create-dir-title" maxWidth="sm" fullWidth>
      <DialogTitle id="create-dir-title" component="h2">{t('dirs.createTitle')}</DialogTitle>
      <form onSubmit={submit} noValidate>
        <DialogContent>
          <TextField
            label={t('dirs.nameLabel')}
            value={name}
            onChange={(e) => setName(e.target.value)}
            error={err != null}
            helperText={err ?? t('kbList.oneLevelHint')}
            aria-invalid={err != null}
            required
            autoFocus
            fullWidth
          />
        </DialogContent>
        <DialogActions>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          <Button type="submit" variant="contained" disabled={submitting}>{t('common.create')}</Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// —— 条目（状态筛选） ——

function summarize(text: string, len = 120): string {
  const s = text.replace(/\s+/g, ' ').trim();
  return s.length > len ? `${s.slice(0, len)}…` : s;
}

function ItemsPanel({ kb, status, page, pageSize, onStatus, onPage }: {
  kb: string;
  status: string;
  page: number;
  pageSize: number;
  onStatus: (s: string) => void;
  onPage: (page: number, pageSize: number) => void;
}) {
  const { t } = useTranslation();
  const points = useApi(
    (signal) => api.points(kb, status, page, pageSize, signal),
    [kb, status, page, pageSize],
  );
  const [expanded, setExpanded] = useState<number | null>(null);
  const items = points.data?.items ?? [];
  const total = points.data?.total;
  const legacy = total == null; // 后端未部署分页契约 → 降级标注
  const maxPage = total != null ? Math.max(1, Math.ceil(total / pageSize)) : 1;

  // 越界页码规范化回仍存在的末页（replace，不产生多余历史项）。
  useEffect(() => {
    if (total != null && page > maxPage) onPage(maxPage, pageSize);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total, maxPage, page, pageSize]);

  return (
    <Stack spacing={1.5}>
      <Stack direction="row" alignItems="center" spacing={1} flexWrap="wrap">
        <FormControl sx={{ minWidth: 200 }} size="small">
          <InputLabel id="status-filter-label">{t('items.statusFilter')}</InputLabel>
          <Select
            labelId="status-filter-label"
            label={t('items.statusFilter')}
            value={status}
            onChange={(e) => onStatus(e.target.value)}
          >
            {TASK_STATUSES.map((s) => (
              <MenuItem key={s} value={s}>{t(`status.${s}`)}</MenuItem>
            ))}
          </Select>
        </FormControl>
        {legacy && items.length > 0 && (
          <Typography variant="body2" color="warning.main" role="note">
            {t('items.legacyOnly', { n: items.length })}
          </Typography>
        )}
      </Stack>

      {points.loading ? (
        <LoadingState />
      ) : points.error != null ? (
        <ErrorState error={points.error} onRetry={points.retry} />
      ) : items.length === 0 ? (
        <EmptyState message={t('items.empty')} hint={t('items.emptyFilteredHint')} />
      ) : (
        <>
          <TableContainer>
            <Table size="small" aria-label={t('kbDetail.tab.items')}>
              <TableHead>
                <TableRow>
                  <TableCell sx={{ width: 56 }} aria-label={t('common.expand')} />
                  <TableCell>ID</TableCell>
                  <TableCell>{t('common.task')}</TableCell>
                  <TableCell>{t('common.status')}</TableCell>
                  <TableCell>{t('common.originalText')}</TableCell>
                  <TableCell>{t('common.failure')}</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {items.map((it) => (
                  <ItemRow
                    key={it.ID}
                    item={it}
                    expanded={expanded === it.ID}
                    onToggle={() => setExpanded(expanded === it.ID ? null : it.ID)}
                  />
                ))}
              </TableBody>
            </Table>
          </TableContainer>
          <TablePagination
            component="div"
            count={total ?? items.length}
            page={page - 1}
            onPageChange={(_, p) => onPage(p + 1, pageSize)}
            rowsPerPage={pageSize}
            onRowsPerPageChange={(e) => onPage(1, Number.parseInt(e.target.value, 10))}
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

function ItemRow({ item, expanded, onToggle }: { item: TaskItem; expanded: boolean; onToggle: () => void }) {
  const { t } = useTranslation();
  const failure = item.Error?.Valid ? item.Error.String : '';
  const handleKey = (e: ReactKeyboardEvent) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onToggle();
    }
  };
  return (
    <>
      <TableRow hover aria-expanded={expanded}>
        <TableCell sx={{ width: 56 }}>
          <IconButton
            size="small"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={expanded ? t('common.collapse') : t('common.expand')}
          >
            <svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" style={{ transform: expanded ? 'rotate(180deg)' : undefined, transition: 'transform .15s' }}>
              <path d="M7 10l5 5 5-5H7z" fill="currentColor" />
            </svg>
          </IconButton>
        </TableCell>
        {/* 整行亦可点击展开：键盘 Enter/Space 与读屏 role 均可达。 */}
        <TableCell
          tabIndex={0}
          role="button"
          onKeyDown={handleKey}
          onClick={onToggle}
          sx={{ cursor: 'pointer', userSelect: 'none' }}
        >
          {item.ID}
        </TableCell>
        <TableCell
          tabIndex={0}
          role="button"
          onKeyDown={handleKey}
          onClick={onToggle}
          sx={{ cursor: 'pointer' }}
        >
          {item.TaskID}
        </TableCell>
        <TableCell><StatusChip status={item.Status} /></TableCell>
        <TableCell
          tabIndex={0}
          role="button"
          onKeyDown={handleKey}
          onClick={onToggle}
          sx={{ cursor: 'pointer', maxWidth: 420 }}
        >
          {summarize(item.OriginalText)}
        </TableCell>
        <TableCell sx={{ maxWidth: 220 }}>{failure ? summarize(failure, 80) : '—'}</TableCell>
      </TableRow>
      {expanded && (
        <TableRow>
          <TableCell colSpan={6} sx={{ bgcolor: 'grey.50' }}>
            <Stack spacing={1} sx={{ py: 0.5 }}>
              <div>
                <Typography variant="body2" fontWeight={600}>{t('common.originalText')}</Typography>
                <Typography variant="body2" sx={{ whiteSpace: 'pre-wrap' }}>{item.OriginalText}</Typography>
              </div>
              {item.CleanText?.Valid && (
                <div>
                  <Typography variant="body2" fontWeight={600}>{t('common.cleanText')}</Typography>
                  <Typography variant="body2" sx={{ whiteSpace: 'pre-wrap' }}>{item.CleanText.String}</Typography>
                </div>
              )}
              {failure && (
                <div>
                  <Typography variant="body2" fontWeight={600}>{t('common.failure')}</Typography>
                  <Typography variant="body2" color="error">{failure}</Typography>
                </div>
              )}
            </Stack>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

// —— 上传 ——

function UploadDialog({ open, kb, onClose }: { open: boolean; kb: string; onClose: () => void }) {
  const { t } = useTranslation();
  const [files, setFiles] = useState<{ name: string; text: string }[]>([]);
  const [texts, setTexts] = useState('');
  const [dir, setDir] = useState('');
  const [dirErr, setDirErr] = useState<string | null>(null);
  const [inputErr, setInputErr] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [notReady, setNotReady] = useState(false);
  const [taskID, setTaskID] = useState<number | null>(null);

  const pastedTexts = useMemo(
    () => texts.split(/\n\s*\n/).map((s) => s.trim()).filter((s) => s !== ''),
    [texts],
  );

  async function onPick(fList: FileList | null) {    const loaded: { name: string; text: string }[] = [];
    for (const f of Array.from(fList ?? [])) {
      loaded.push({ name: f.name, text: await f.text() });
    }
    setFiles(loaded);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    const d = dir.trim();
    if (d === '' || d.includes('/')) { setDirErr(t('upload.invalidDir')); return; }
    if (files.length === 0 && pastedTexts.length === 0) { setInputErr(t('upload.emptyInput')); return; }
    setDirErr(null);
    setInputErr(null);
    setSubmitting(true);
    setNotReady(false);
    try {
      const res = await api.uploadDocs(kb, {
        dir: d,
        texts: pastedTexts.length > 0 ? pastedTexts : undefined,
        files: files.length > 0 ? files : undefined,
      });
      setTaskID(res.task_id ?? null);
    } catch (ex) {
      if (ex instanceof ApiError && (ex.status === 501 || ex.status === 503)) {
        setNotReady(true); // 保留输入
      } else {
        setInputErr(ex instanceof ApiError ? ex.message : String(ex));
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onClose={onClose} aria-labelledby="upload-title" maxWidth="md" fullWidth>
      <DialogTitle id="upload-title" component="h2">{t('upload.title')}</DialogTitle>
      <form onSubmit={submit} noValidate>
        <DialogContent>
          <Stack spacing={2}>
            {notReady && <Alert severity="info">{t('upload.notReady')}</Alert>}
            {taskID != null && <TaskProgress kb={kb} taskID={taskID} />}
            <Button variant="outlined" component="label">
              {t('upload.pickFiles')}
              <input
                type="file"
                multiple
                hidden
                onChange={(e) => void onPick(e.target.files)}
                aria-label={t('upload.fileLabel')}
              />
            </Button>
            {files.length > 0 && (
              <Typography variant="body2" color="text.secondary">
                {files.map((f) => f.name).join(', ')} · {t('upload.fileHelp')}
              </Typography>
            )}
            <TextField
              label={t('upload.textsLabel')}
              value={texts}
              onChange={(e) => setTexts(e.target.value)}
              helperText={t('upload.textsHelp')}
              multiline
              minRows={4}
              fullWidth
            />
            <TextField
              label={t('upload.dirLabel')}
              value={dir}
              onChange={(e) => setDir(e.target.value)}
              error={dirErr != null}
              helperText={dirErr ?? t('upload.dirHelp')}
              aria-invalid={dirErr != null}
              required
              fullWidth
            />
            {inputErr != null && (
              <Typography variant="body2" color="error" role="alert" aria-describedby="upload-title">
                {inputErr}
              </Typography>
            )}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={onClose}>{t('common.close')}</Button>
          <Button type="submit" variant="contained" disabled={submitting}>
            {submitting ? t('upload.submitting') : t('upload.submit')}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

// 上传后任务进度：3s 轮询至终态（cleaned/failed）。
function TaskProgress({ kb, taskID }: { kb: string; taskID: number }) {
  const { t } = useTranslation();
  const task = useApi((signal) => api.task(kb, taskID, signal), [kb, taskID], { pollMs: 3000 });
  const items = task.data?.Items ?? [];
  const terminal = ['cleaned', 'failed'].includes(task.data?.Status ?? '');
  const done = items.filter((i) => i.Status === 'cleaned' || i.Status === 'failed').length;
  const failed = items.filter((i) => i.Status === 'failed').length;

  if (task.error != null) {
    return <Alert severity="warning">{`${t('common.errorPrefix')}：${task.error.message}`}</Alert>;
  }
  if (task.data == null) {
    return (
      <Stack spacing={0.5} role="status">
        <LinearProgress aria-label={t('common.loading')} />
        <Typography variant="body2" color="text.secondary">
          {t('upload.taskQueued', { task: taskID })}
        </Typography>
      </Stack>
    );
  }
  return (
    <Stack spacing={0.5} role="status">
      {!terminal && <LinearProgress aria-label={t('common.loading')} />}
      <Typography variant="body2">
        {t('upload.progress', { task: taskID, done, total: items.length, failed })}
        {!terminal && <> · {t('upload.taskQueued', { task: taskID })}</>}
      </Typography>
    </Stack>
  );
}

export function NotFoundPage() {
  const { t } = useTranslation();
  const location = useLocation();
  useDocumentTitle(t('docTitle.notFound'));
  return (
    <Stack spacing={1.5} sx={{ mt: 4 }}>
      <Typography variant="h1">{t('notFound.title')}</Typography>
      <Typography variant="body2" color="text.secondary">{t('notFound.detail', { url: location.pathname + location.search })}</Typography>
      <Button component={RouterLink} to="/" variant="outlined" sx={{ alignSelf: 'flex-start' }}>
        {t('notFound.back')}
      </Button>
    </Stack>
  );
}
