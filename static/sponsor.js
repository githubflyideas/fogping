// Remote sponsor carousel.
//
// The sponsor slot can show up to three image slides listed in a manifest kept
// in https://github.com/githubflyideas/ads-sponsor, so the slides can change
// without a new release. The browser fetches the manifest directly; the server
// never phones home. Anything short of a manifest that parses and at least one
// image that actually loads leaves the built-in sponsor text exactly as it was:
// offline, intranet, blocked, slow, malformed — all fail closed.
//
// Usage: loadSponsor('pingping')
(function () {
  'use strict';
  var BASE = 'https://raw.githubusercontent.com/githubflyideas/ads-sponsor/main/';
  var TIMEOUT_MS = 3000, ROTATE_MS = 5500, MAX = 3;

  function httpURL(v, base, httpsOnly) {
    if (typeof v !== 'string' || !v.trim()) return null;
    try {
      var u = new URL(v.trim(), base);
      if (u.protocol === 'https:' || (!httpsOnly && u.protocol === 'http:')) return u.href;
    } catch (e) {}
    return null;
  }
  function str(v, n) { return typeof v === 'string' ? v.slice(0, n) : ''; }

  function loadImg(src) {
    return new Promise(function (ok, bad) {
      var img = new Image();
      img.decoding = 'async';
      img.referrerPolicy = 'no-referrer';
      img.onload = function () { img.naturalWidth > 0 ? ok(img) : bad(); };
      img.onerror = bad;
      img.src = src;
    });
  }
  function settle(ps) {
    return Promise.all(ps.map(function (p) {
      return p.then(function (v) { return v; }, function () { return null; });
    }));
  }
  function withTimeout(p, ms) {
    return Promise.race([p, new Promise(function (_, bad) { setTimeout(bad, ms); })]);
  }

  var CSS = '.spc{position:relative;max-width:728px}' +
    '.spc-track{position:relative;aspect-ratio:728/90;border-radius:8px;overflow:hidden;background:var(--panel)}' +
    '.spc-slide{position:absolute;inset:0;opacity:0;transition:opacity .45s ease;pointer-events:none;display:block}' +
    '.spc-slide.on{opacity:1;pointer-events:auto}' +
    '.spc-slide img{width:100%;height:100%;object-fit:cover;display:block}' +
    '.spc-dots{display:flex;gap:7px;margin-top:8px;align-items:center}' +
    '.spc-dot{width:7px;height:7px;padding:0;border-radius:50%;border:0;cursor:pointer;background:var(--line);opacity:.9}' +
    '.spc-dot.on{background:var(--phos)}' +
    '.spc-title{color:var(--dim);font-size:var(--fs-small,12px);margin-left:8px}';

  function build(slot, slides) {
    var st = document.createElement('style');
    st.textContent = CSS;
    document.head.appendChild(st);

    var box = document.createElement('div');
    box.className = 'promo sponsor';
    box.id = 'sponsorRemote';
    var head = slot.querySelector('.promo-h');
    if (head) box.appendChild(head.cloneNode(true));
    // the cloned header must not duplicate an id (#ver) the page still writes to
    box.querySelectorAll('[id]').forEach(function (e) {
      var orig = document.getElementById(e.id);
      e.removeAttribute('id');
      if (orig && orig !== e) {
        new MutationObserver(function () { e.textContent = orig.textContent; })
          .observe(orig, { childList: true, characterData: true, subtree: true });
        e.textContent = orig.textContent;
      }
    });

    var car = document.createElement('div'); car.className = 'spc';
    var track = document.createElement('div'); track.className = 'spc-track';
    var dots = document.createElement('div'); dots.className = 'spc-dots';
    var title = document.createElement('span'); title.className = 'spc-title';
    var els = [], ds = [];
    slides.forEach(function (s, i) {
      var a = document.createElement(s.link ? 'a' : 'div');
      a.className = 'spc-slide';
      if (s.link) { a.href = s.link; a.target = '_blank'; a.rel = 'noopener noreferrer sponsored'; }
      s.img.alt = s.alt;
      if (s.title) a.title = s.title;
      a.appendChild(s.img);
      track.appendChild(a); els.push(a);
      if (slides.length > 1) {
        var d = document.createElement('button');
        d.type = 'button'; d.className = 'spc-dot';
        d.setAttribute('aria-label', String(i + 1));
        d.onclick = function () { show(i); };
        dots.appendChild(d); ds.push(d);
      }
    });
    dots.appendChild(title);
    car.appendChild(track); car.appendChild(dots);
    box.appendChild(car);

    var cur = 0, paused = false;
    function show(i) {
      cur = i;
      els.forEach(function (e, j) { e.classList.toggle('on', j === i); });
      ds.forEach(function (e, j) { e.classList.toggle('on', j === i); });
      title.textContent = slides[i].title || '';
    }
    show(0);
    if (slides.length > 1) {
      car.addEventListener('mouseenter', function () { paused = true; });
      car.addEventListener('mouseleave', function () { paused = false; });
      car.addEventListener('focusin', function () { paused = true; });
      car.addEventListener('focusout', function () { paused = false; });
      setInterval(function () {
        if (!paused && !document.hidden) show((cur + 1) % slides.length);
      }, ROTATE_MS);
    }
    slot.parentNode.insertBefore(box, slot);
    slot.style.display = 'none';
  }

  window.loadSponsor = function (product) {
    if (!/^[a-z0-9-]+$/.test(product)) return;
    var slot = document.getElementById('sponsorSlot');
    if (!slot || !window.fetch || !window.URL) return;
    var url = BASE + product + '/sponsors.json';
    var ctl = window.AbortController ? new AbortController() : null;
    var timer = setTimeout(function () { if (ctl) ctl.abort(); }, TIMEOUT_MS);
    fetch(url, { signal: ctl ? ctl.signal : undefined, credentials: 'omit',
                 referrerPolicy: 'no-referrer', cache: 'no-cache' })
      .then(function (r) { if (!r.ok) throw 0; return r.json(); })
      .then(function (m) {
        clearTimeout(timer);
        var list = m && Array.isArray(m.slides) ? m.slides : [];
        var want = [];
        for (var i = 0; i < list.length && want.length < MAX; i++) {
          var s = list[i] || {};
          var img = httpURL(s.image, url, true);
          if (!img) continue;
          want.push({ src: img, link: httpURL(s.link, url, false),
                      alt: str(s.alt, 200), title: str(s.title, 120) });
        }
        if (!want.length) return;
        return withTimeout(settle(want.map(function (w) { return loadImg(w.src); })), TIMEOUT_MS)
          .then(function (imgs) {
            var ok = [];
            want.forEach(function (w, i) { if (imgs[i]) { w.img = imgs[i]; ok.push(w); } });
            if (ok.length) build(slot, ok);
          });
      })
      .catch(function () { clearTimeout(timer); /* keep the built-in text */ });
  };
})();
