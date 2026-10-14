import Chip from '@mui/material/Chip';
import { useTranslation } from 'react-i18next';
import type { TaskStatus } from '../api';

const COLOR: Record<TaskStatus, 'default' | 'info' | 'warning' | 'success' | 'error'> = {
  pending: 'default',
  embedded: 'info',
  cleaning: 'warning',
  cleaned: 'success',
  failed: 'error',
};

/** 状态 Chip：颜色 + 文字双通道，不单靠颜色传达状态。 */
export function StatusChip({ status }: { status: TaskStatus }) {
  const { t } = useTranslation();
  return <Chip label={t(`status.${status}`)} color={COLOR[status]} size="small" />;
}

export function normalizeStatus(status: string): TaskStatus {
  return (TASK_STATUS_SET as readonly string[]).includes(status) ? (status as TaskStatus) : 'cleaning';
}

const TASK_STATUS_SET = ['pending', 'embedded', 'cleaning', 'cleaned', 'failed'] as const;
