//go:build e2e_ui

package ui

// supervisionCursor is injected before every page load. It paints a dot that
// tracks the mouse and a ripple on each click, so when a human supervises a
// headed run (or reviews the video) they can see exactly what Playwright is
// doing. It is purely decorative: pointer-events:none, top z-index, and it
// self-skips if already installed, so it never interferes with the page under
// test.
const supervisionCursor = `(function () {
  if (window.__pwSupervisor) return;
  window.__pwSupervisor = true;
  var css =
    '#pw-cursor{position:fixed;top:0;left:0;width:18px;height:18px;margin:-9px 0 0 -9px;' +
    'border-radius:50%;background:rgba(11,98,214,.30);border:2px solid #0b62d6;' +
    'pointer-events:none;z-index:2147483647;transition:transform .04s linear;mix-blend-mode:multiply}' +
    '.pw-ripple{position:fixed;border-radius:50%;border:2px solid #0b62d6;pointer-events:none;' +
    'z-index:2147483646;animation:pwrip .6s ease-out forwards}' +
    '@keyframes pwrip{from{width:0;height:0;opacity:.85}to{width:64px;height:64px;margin:-32px 0 0 -32px;opacity:0}}';
  var style = document.createElement('style');
  style.textContent = css;
  (document.head || document.documentElement).appendChild(style);

  function boot() {
    if (document.getElementById('pw-cursor')) return;
    var dot = document.createElement('div');
    dot.id = 'pw-cursor';
    document.body.appendChild(dot);
    window.addEventListener('mousemove', function (e) {
      dot.style.transform = 'translate(' + e.clientX + 'px,' + e.clientY + 'px)';
    });
    document.addEventListener('click', function (e) {
      var r = document.createElement('div');
      r.className = 'pw-ripple';
      r.style.left = e.clientX + 'px';
      r.style.top = e.clientY + 'px';
      document.body.appendChild(r);
      setTimeout(function () { r.remove(); }, 650);
    });
  }
  if (document.body) boot();
  else document.addEventListener('DOMContentLoaded', boot);
})();`
