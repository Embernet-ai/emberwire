// The one place a theme is applied.
//
// The editor follows the Industrial Dashboard's mechanism, a dark-mode class on
// body, so that it feels native inside the dashboard's iframe. The HotLoop tokens
// flip on data-theme instead. Setting both here, together, is what keeps the two
// from ever disagreeing.
export function setDarkMode(dark: boolean): void {
  document.body.classList.toggle('dark-mode', dark);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
}
