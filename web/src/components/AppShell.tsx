import AppBar from '@mui/material/AppBar';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import Toolbar from '@mui/material/Toolbar';
import Typography from '@mui/material/Typography';
import { useTranslation } from 'react-i18next';
import { Link as RouterLink, NavLink, Outlet } from 'react-router-dom';

/** 应用外壳：单行窄工具栏（窄视口允许换行）+ 内容区；skip link + aria-current 导航。 */
export function AppShell() {
  const { t } = useTranslation();
  const navSx = {
    px: 1.5,
    color: 'text.secondary',
    '&[aria-current="page"]': {
      color: 'primary.main',
      fontWeight: 600,
      textDecoration: 'underline',
      textUnderlineOffset: 6,
    },
  } as const;
  return (
    <Box sx={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      <Box
        component="a"
        href="#main-content"
        sx={{
          position: 'absolute',
          left: -9999,
          top: 0,
          bgcolor: 'background.paper',
          p: 1,
          zIndex: 2000,
          '&:focus': { left: 8 },
        }}
      >
        {t('a11y.skipToMain')}
      </Box>
      <AppBar position="static" color="transparent" elevation={0}>
        <Toolbar
          variant="dense"
          sx={{
            borderBottom: 1,
            borderColor: 'divider',
            bgcolor: 'background.paper',
            gap: 0.5,
            rowGap: 0.25,
            flexWrap: 'wrap',
          }}
        >
          <Typography
            component={RouterLink}
            to="/"
            variant="h2"
            sx={{ color: 'primary.main', textDecoration: 'none', mr: 2 }}
          >
            {t('app.name')}
          </Typography>
          <Stack direction="row" component="nav" aria-label={t('app.tagline')} sx={{ gap: 0.5 }}>
            <Button component={NavLink} to="/" end sx={navSx} aria-current={undefined}>
              {t('nav.kbs')}
            </Button>
            <Button component={NavLink} to="/cleaning" sx={navSx}>
              {t('nav.cleaning')}
            </Button>
          </Stack>
          <Box sx={{ flex: 1 }} />
          <Button component={RouterLink} to="/login">
            {t('nav.login')}
          </Button>
        </Toolbar>
      </AppBar>
      <Box component="main" id="main-content" sx={{ flex: 1, px: { xs: 1.5, sm: 3 }, py: 2, maxWidth: 1100, width: '100%', mx: 'auto' }}>
        <Outlet />
      </Box>
    </Box>
  );
}
