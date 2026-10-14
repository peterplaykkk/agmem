// agmem web — 明亮单主题（light-only，无暗色分支）。
import { createTheme } from '@mui/material/styles';

export const theme = createTheme({
  colorSchemes: { light: true, dark: undefined },
  cssVariables: true,
  palette: {
    mode: 'light',
    primary: { main: '#175cd3', dark: '#134fae', light: '#d1e0ff' },
    secondary: { main: '#175cd3' },
    success: { main: '#067647', contrastText: '#ffffff' },
    warning: { main: '#b54708' },
    error: { main: '#b42318' },
    background: { default: '#f2f4f7', paper: '#ffffff' },
    text: { primary: '#101828', secondary: '#475467', disabled: '#98a2b3' },
    divider: '#e4e7ec',
  },
  shape: { borderRadius: 8 },
  typography: {
    fontFamily:
      'system-ui, -apple-system, "Segoe UI", Roboto, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif',
    h1: { fontSize: '1rem', fontWeight: 600, lineHeight: 1.4, margin: 0 },
    h2: { fontSize: '0.9375rem', fontWeight: 600, margin: 0 },
    body2: { fontSize: '0.8125rem' },
    button: { textTransform: 'none' },
  },
  components: {
    MuiButton: { defaultProps: { size: 'small' } },
    MuiChip: { defaultProps: { size: 'small' } },
    MuiTextField: { defaultProps: { size: 'small' } },
  },
});
