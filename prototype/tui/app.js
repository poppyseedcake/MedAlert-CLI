const variants = [
  { key: "A", name: "Centrum stanu" },
  { key: "B", name: "Przepływ zadaniowy" },
  { key: "C", name: "Przegląd obiektów" },
];

const screens = {
  home: {
    title: "Stan systemu",
    lead: "Najważniejsze informacje przed wykonaniem zadania administracyjnego.",
  },
  accounts: {
    title: "Konta Medicover",
    lead: "Każde konto ma oddzielny stan uwierzytelnienia i własne profile obserwacji.",
  },
  profiles: {
    title: "Profile obserwacji",
    lead: "Profil łączy kryteria wyszukiwania, częstotliwość oraz trasy powiadomień.",
  },
  routes: {
    title: "Trasy powiadomień",
    lead: "Każda trasa ma niezależny wynik dostarczenia i chronione źródło sekretu.",
  },
  monitoring: {
    title: "Sprawdzenie i monitoring",
    lead: "Ręczne sprawdzenie wykonuje jeden pełny przebieg. Stały monitoring działa w systemd.",
  },
  history: {
    title: "Historia",
    lead: "Historia pokazuje przebiegi, incydenty oraz próby dostarczenia bez danych tajnych.",
  },
  auth: {
    title: "Uwierzytelnij konto Domowe",
    lead: "MedAlert prowadzi przez logowanie i zapisuje sesję w chronionym magazynie.",
  },
  accountAdd: {
    title: "Dodaj konto Medicover",
    lead: "Zapisz zwykłe dane konta. Hasło podasz dopiero podczas bezpiecznego uwierzytelnienia.",
  },
  profileEdit: {
    title: "Edytuj profil Mokotów — internista",
    lead: "Zmień filtry wyszukiwania i częstotliwość jednego profilu obserwacji.",
  },
  routeEdit: {
    title: "Napraw trasę Zapasowa",
    lead: "Wskaż chronione źródło sekretu. Wartość sekretu nie trafia do bazy danych.",
  },
  service: {
    title: "Usługa systemd użytkownika",
    lead: "Stały monitoring działa poza interfejsem terminalowym i nie wymaga otwartej sesji TUI.",
  },
};

const state = {
  variant: readVariant(),
  screen: "home",
  notice: "Gotowe",
  selectedObject: "profile:mokotow",
  lastAction: "brak",
  authStep: "password",
  helpOpen: false,
  focusAfterRender: null,
};

function readVariant() {
  const requested = new URLSearchParams(window.location.search).get("variant")?.toUpperCase();
  return variants.some((variant) => variant.key === requested) ? requested : "A";
}

function setVariant(next) {
  state.variant = next;
  const url = new URL(window.location.href);
  url.searchParams.set("variant", next);
  window.history.replaceState({}, "", url);
  render();
}

function cycleVariant(delta) {
  const current = variants.findIndex((variant) => variant.key === state.variant);
  const next = (current + delta + variants.length) % variants.length;
  setVariant(variants[next].key);
}

function menuItems() {
  return [
    ["1", "home", "Stan"],
    ["2", "accounts", "Konta"],
    ["3", "profiles", "Profile"],
    ["4", "routes", "Powiadomienia"],
    ["5", "history", "Historia"],
  ];
}

function menuMarkup() {
  return `<ul class="menu">
    ${menuItems()
      .map(
        ([key, screen, label]) => `<li><button type="button" data-screen="${screen}" ${
          state.screen === screen ? 'aria-current="page"' : ""
        } data-focus-id="screen:${screen}"><span class="menu-index">${key}</span>${label}</button></li>`,
      )
      .join("")}
    <li><button type="button" data-screen="monitoring" data-focus-id="screen:monitoring"><span class="menu-index">M</span>Sprawdź teraz</button></li>
  </ul>`;
}

function overviewContent() {
  return `
    <h2 class="content-title">${screens[state.screen]?.title ?? screens.home.title}</h2>
    <p class="content-lede">${screens[state.screen]?.lead ?? screens.home.lead}</p>
    ${screenBody(state.screen)}
  `;
}

function screenBody(screen) {
  if (screen === "accounts") {
    return `<table class="status-table">
      <thead><tr><th>Konto</th><th>Stan</th><th>Profile</th></tr></thead>
      <tbody>
        <tr><td>Domowe</td><td class="state-warn">Wymaga uwierzytelnienia</td><td>2 aktywne</td></tr>
        <tr><td>Rodzice</td><td class="state-good">Sesja aktywna</td><td>1 aktywny</td></tr>
      </tbody>
    </table>
    <div class="actions"><button class="action action-primary" type="button" data-action="authenticate" data-focus-id="action:authenticate">Uwierzytelnij Domowe</button><button class="action" type="button" data-action="account-add">Dodaj konto</button></div>`;
  }

  if (screen === "profiles") {
    return `<table class="status-table">
      <thead><tr><th>Profil</th><th>Konto</th><th>Interwał</th><th>Ostatni przebieg</th></tr></thead>
      <tbody>
        <tr><td>Mokotów — internista</td><td>Domowe</td><td>5 min</td><td class="state-warn">wstrzymany</td></tr>
        <tr><td>Centrum — okulista</td><td>Domowe</td><td>10 min</td><td>08:42 · brak terminów</td></tr>
        <tr><td>Gdańsk — kardiolog</td><td>Rodzice</td><td>15 min</td><td class="state-good">08:39 · 1 termin</td></tr>
      </tbody>
    </table>
    <div class="actions"><button class="action action-primary" type="button" data-action="profile-add">Dodaj profil</button><button class="action" type="button" data-action="profile-edit">Edytuj filtry wybranego</button><button class="action" type="button" data-action="profile-disable">Wyłącz</button></div>`;
  }

  if (screen === "routes") {
    return `<table class="status-table">
      <thead><tr><th>Trasa</th><th>Dostawca</th><th>Sekret</th><th>Ostatnia próba</th></tr></thead>
      <tbody>
        <tr><td>Telefon</td><td>Telegram</td><td class="state-good">dostępny</td><td class="state-good">dostarczono</td></tr>
        <tr><td>Serwer</td><td>Gotify</td><td class="state-good">dostępny</td><td class="state-good">dostarczono</td></tr>
        <tr><td>Zapasowa</td><td>Pushover</td><td class="state-bad">brak</td><td>—</td></tr>
      </tbody>
    </table>
    <div class="actions"><button class="action action-primary" type="button" data-action="test-route">Wyślij test</button><button class="action" type="button" data-action="route-edit">Napraw sekret</button></div>`;
  }

  if (screen === "monitoring") {
    return `<p class="empty-note"><strong>Zakres:</strong> 3 aktywne profile. Profil „Mokotów — internista” poczeka na uwierzytelnienie. Pozostałe profile mogą działać niezależnie.</p>
    <div class="actions"><button class="action action-primary" type="button" data-action="run-check">Wykonaj jedno sprawdzenie</button><button class="action" type="button" data-action="service-show">Pokaż usługę systemd</button></div>`;
  }

  if (screen === "history") {
    return `<ul class="activity">
      <li><time>08:42</time> Centrum — okulista: przebieg zakończony, brak terminów</li>
      <li><time>08:39</time> Gdańsk — kardiolog: znaleziono 1 termin</li>
      <li><time>08:39</time> Telefon: powiadomienie dostarczone</li>
      <li><time>08:31</time> Domowe: wymagane uwierzytelnienie</li>
    </ul>`;
  }

  if (screen === "auth") {
    if (state.authStep === "mfa") {
      return `<div class="form-grid"><label for="mfa-code">Kod MFA</label><input id="mfa-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" aria-describedby="mfa-note" /><span></span><span id="mfa-note" class="state-warn">Kod ma sześć cyfr i nie zostanie zapisany.</span></div>
      <div class="actions"><button class="action action-primary" type="button" data-action="finish-auth" data-focus-id="action:finish-auth">Zakończ uwierzytelnienie</button><button class="action" type="button" data-action="auth-back">Wróć</button><button class="action" type="button" data-screen="accounts">Anuluj</button></div>`;
    }
    return `<div class="form-grid"><label for="account-password">Hasło</label><input id="account-password" type="password" autocomplete="current-password" /><span></span><label class="check-row"><input type="checkbox" /> Zapisz hasło w Secret Service</label></div>
    <p class="empty-note">MedAlert pokaże wybór metody i zaufanego urządzenia tylko wtedy, gdy Medicover je udostępni.</p>
    <div class="actions"><button class="action action-primary" type="button" data-action="auth-next" data-focus-id="action:auth-next">Dalej</button><button class="action" type="button" data-screen="accounts">Anuluj</button></div>`;
  }

  if (screen === "accountAdd") {
    return `<div class="form-grid"><label for="account-name">Nazwa konta</label><input id="account-name" value="Praca" /><label for="account-login">Login Medicover</label><input id="account-login" autocomplete="username" placeholder="adres e-mail lub login" /></div>
    <div class="actions"><button class="action action-primary" type="button" data-action="account-save">Zapisz i uwierzytelnij</button><button class="action" type="button" data-screen="accounts">Anuluj</button></div>`;
  }

  if (screen === "profileEdit") {
    return `<div class="form-grid"><label for="specialty">Specjalizacja</label><input id="specialty" value="Internista" /><label for="clinic">Placówki</label><input id="clinic" value="Mokotów, Wilanów" /><label for="doctor">Lekarze</label><input id="doctor" value="Dowolny" /><label for="visit-type">Typ wizyty</label><select id="visit-type"><option>W placówce</option><option>Teleporada</option></select><label for="date-range">Zakres dat</label><input id="date-range" value="najbliższe 30 dni" /><label for="interval">Interwał</label><input id="interval" value="5 min" /></div>
    <div class="actions"><button class="action action-primary" type="button" data-action="profile-save">Zapisz filtry</button><button class="action" type="button" data-screen="profiles">Anuluj</button></div>`;
  }

  if (screen === "routeEdit") {
    return `<div class="form-grid"><label for="provider">Dostawca</label><select id="provider"><option>Pushover</option><option>Telegram</option><option>Gotify</option></select><label for="secret-source">Źródło sekretu</label><select id="secret-source"><option>Secret Service</option><option>Chroniony plik</option><option>Tylko teraz</option></select><label for="secret-value">Nowy sekret</label><input id="secret-value" type="password" autocomplete="new-password" /></div>
    <div class="actions"><button class="action action-primary" type="button" data-action="route-save">Zapisz i wyślij test</button><button class="action" type="button" data-screen="routes">Anuluj</button></div>`;
  }

  if (screen === "service") {
    return `<ul class="facts"><li><span class="detail-key">Stan</span><span class="state-good">aktywna</span></li><li><span class="detail-key">Jednostka</span>medalert.service</li><li><span class="detail-key">Ostatni start</span>dzisiaj, 06:24</li><li><span class="detail-key">Następny cykl</span>za 2 min 14 s</li></ul><div class="actions"><button class="action action-primary" type="button" data-action="service-restart">Uruchom ponownie</button><button class="action" type="button" data-action="service-log">Pokaż bezpieczny dziennik</button></div>`;
  }

  return `<table class="status-table">
    <thead><tr><th>Obszar</th><th>Stan</th><th>Następne działanie</th></tr></thead>
    <tbody>
      <tr><td>Konta</td><td class="state-warn">1 z 2 wymaga uwierzytelnienia</td><td>Uwierzytelnij „Domowe”</td></tr>
      <tr><td>Profile</td><td class="state-good">3 aktywne</td><td>Brak</td></tr>
      <tr><td>Powiadomienia</td><td class="state-bad">1 trasa bez sekretu</td><td>Napraw „Zapasowa”</td></tr>
      <tr><td>Monitoring</td><td class="state-good">systemd działa od 2 godz. 18 min</td><td>Brak</td></tr>
    </tbody>
  </table>
  <h3 class="section-heading">Ostatnia aktywność</h3>
  <ul class="activity"><li><time>08:42</time> Przebieg „Centrum — okulista”: brak terminów</li><li><time>08:39</time> Dostarczono nowy termin przez Telegram</li></ul>`;
}

function renderVariantA() {
  return `<div class="variant-a">
    <nav class="pane" aria-label="Główne obszary"><p class="pane-title">Obszary</p>${menuMarkup()}</nav>
    <section class="pane">${overviewContent()}</section>
    <aside class="pane"><p class="pane-title">Do zrobienia</p>
      <div class="summary-block"><span class="summary-label">WYMAGA UWAGI</span><span class="summary-value state-warn">Konto Domowe</span><div class="actions"><button class="action" data-action="authenticate" data-focus-id="action:authenticate">Uwierzytelnij</button></div></div>
      <div class="summary-block"><span class="summary-label">NASTĘPNY PRZEBIEG</span><span class="summary-value">za 2 min 14 s</span></div>
      <div class="summary-block"><span class="summary-label">USŁUGA</span><span class="summary-value state-good">systemd aktywna</span></div>
    </aside>
  </div>`;
}

function renderVariantB() {
  const tasks = [
    ["home", "Sprawdź, czy wszystko działa", "Stan kont, profili, tras i usługi"],
    ["accounts", "Napraw dostęp do konta", "Uwierzytelnij lub wyloguj konto"],
    ["profiles", "Zmień obserwowany termin", "Kryteria, interwał i stan profilu"],
    ["routes", "Skonfiguruj powiadomienia", "Trasy, sekrety i wiadomość testowa"],
    ["monitoring", "Wykonaj sprawdzenie lub sprawdź usługę", "Jeden przebieg albo stan systemd"],
    ["history", "Wyjaśnij ostatnie zdarzenie", "Przebiegi, incydenty i dostarczenia"],
  ];
  return `<div class="variant-b">
    <nav class="pane" aria-label="Zadania"><p class="pane-title">Co chcesz zrobić?</p><ul class="task-list">
      ${tasks.map(([screen, label, detail]) => `<li><button type="button" data-screen="${screen}" data-focus-id="screen:${screen}" ${state.screen === screen ? 'aria-current="page"' : ""}><strong>${label}</strong><small>${detail}</small></button></li>`).join("")}
    </ul></nav>
    <section class="pane"><p class="pane-title">Prowadzenie zadania</p>${overviewContent()}
      <div class="flow-preview"><strong>Następny bezpieczny krok</strong><p>${nextStepText()}</p></div>
    </section>
  </div>`;
}

function nextStepText() {
  const steps = {
    home: "Najpierw napraw konto „Domowe”. Pozostałe konto i profile nadal działają.",
    accounts: "Wybierz konto „Domowe”, a następnie rozpocznij uwierzytelnienie.",
    profiles: "Wybierz profil. Zmiana kryteriów nie kończy epizodów terminów, które nadal pasują.",
    routes: "Uzupełnij chroniony sekret trasy „Zapasowa”, a następnie wyślij wiadomość testową.",
    monitoring: "Wykonaj jedno sprawdzenie lub otwórz stan usługi systemd.",
    history: "Otwórz zdarzenie, aby zobaczyć bezpieczne szczegóły i wynik operacji.",
    auth: "Rozpocznij logowanie. MedAlert zapyta tylko o dane, których potrzebuje bieżący etap.",
  };
  return steps[state.screen] ?? steps.home;
}

function renderVariantC() {
  const tree = `<nav class="pane" aria-label="Drzewo obiektów"><p class="pane-title">Obiekty</p><ul class="tree">
      <li class="tree-group">KONTA</li>
      <li><button data-object="account:domowe" data-focus-id="object:account:domowe" ${state.selectedObject === "account:domowe" ? 'aria-current="true"' : ""}>▾ Domowe <span class="state-warn">!</span></button></li>
      <li class="level-2"><button data-object="profile:mokotow" data-focus-id="object:profile:mokotow" ${state.selectedObject === "profile:mokotow" ? 'aria-current="true"' : ""}>├ Mokotów — internista</button></li>
      <li class="level-3"><button data-object="route:telefon" data-focus-id="object:route:telefon" ${state.selectedObject === "route:telefon" ? 'aria-current="true"' : ""}>└ Telefon · Telegram</button></li>
      <li class="level-2"><button data-object="profile:centrum" data-focus-id="object:profile:centrum" ${state.selectedObject === "profile:centrum" ? 'aria-current="true"' : ""}>└ Centrum — okulista</button></li>
      <li><button data-object="account:rodzice" data-focus-id="object:account:rodzice" ${state.selectedObject === "account:rodzice" ? 'aria-current="true"' : ""}>▸ Rodzice</button></li>
      <li class="tree-group">SYSTEM</li>
      <li><button data-object="system:monitoring" data-focus-id="object:system:monitoring" ${state.selectedObject === "system:monitoring" ? 'aria-current="true"' : ""}>Monitoring</button></li>
      <li><button data-object="system:history" data-focus-id="object:system:history" ${state.selectedObject === "system:history" ? 'aria-current="true"' : ""}>Historia</button></li>
    </ul></nav>`;
  const actionScreens = ["auth", "accountAdd", "profileEdit", "routeEdit", "monitoring", "history", "service"];
  if (actionScreens.includes(state.screen)) {
    return `<div class="variant-c">${tree}<section class="pane c-action-pane">${overviewContent()}</section></div>`;
  }
  return `<div class="variant-c">${tree}
    <section class="pane"><p class="pane-title">Lista / relacje</p>${objectRelations()}</section>
    <section class="pane"><p class="pane-title">Szczegóły</p>${objectDetails()}</section>
  </div>`;
}

function objectRelations() {
  if (state.selectedObject.startsWith("account:")) {
    return `<h2 class="content-title">Profile konta</h2><ul class="facts"><li>Mokotów — internista <span class="state-warn">wstrzymany</span></li><li>Centrum — okulista <span class="state-good">aktywny</span></li></ul>`;
  }
  if (state.selectedObject.startsWith("route:")) {
    return `<h2 class="content-title">Trasa i profil</h2><ul class="facts"><li>Profil: Mokotów — internista</li><li>Dostawca: Telegram</li><li>Stan: aktywna</li></ul>`;
  }
  if (state.selectedObject.startsWith("system:")) {
    return `<h2 class="content-title">Ostatnie zdarzenia</h2><ul class="activity"><li><time>08:42</time> brak terminów</li><li><time>08:39</time> 1 termin</li><li><time>08:39</time> dostarczono</li></ul>`;
  }
  return `<h2 class="content-title">Trasy profilu</h2><ul class="facts"><li>Telefon · Telegram <span class="state-good">aktywna</span></li><li>Serwer · Gotify <span class="state-good">aktywna</span></li></ul>`;
}

function objectDetails() {
  const title = {
    "account:domowe": "Konto Domowe",
    "account:rodzice": "Konto Rodzice",
    "profile:mokotow": "Mokotów — internista",
    "profile:centrum": "Centrum — okulista",
    "route:telefon": "Telefon",
    "system:monitoring": "Monitoring",
    "system:history": "Historia",
  }[state.selectedObject];
  let primaryAction = '<button class="action action-primary" type="button" data-action="context-action">Wykonaj główne działanie</button>';
  let extraFact = "";
  if (state.selectedObject === "account:domowe") {
    primaryAction = '<button class="action action-primary" type="button" data-action="authenticate">Uwierzytelnij</button>';
  } else if (state.selectedObject.startsWith("profile:")) {
    primaryAction = '<button class="action action-primary" type="button" data-action="profile-edit">Edytuj filtry</button>';
    extraFact = '<li><span class="detail-key">Filtry</span>specjalizacja, placówka, lekarz, typ i data</li>';
  } else if (state.selectedObject.startsWith("route:")) {
    primaryAction = '<button class="action action-primary" type="button" data-action="test-route">Wyślij test</button>';
    extraFact = '<li><span class="detail-key">Sekret</span><span class="state-good">dostępny w Secret Service</span></li>';
  } else if (state.selectedObject === "system:monitoring") {
    primaryAction = '<button class="action action-primary" type="button" data-action="run-check">Wykonaj sprawdzenie</button>';
  } else if (state.selectedObject === "system:history") {
    primaryAction = '<button class="action action-primary" type="button" data-screen="history">Otwórz historię</button>';
  }
  return `<h2 class="content-title">${title}</h2>
    <ul class="facts">
      <li><span class="detail-key">Stan</span>${state.selectedObject === "account:domowe" ? '<span class="state-warn">wymaga uwierzytelnienia</span>' : '<span class="state-good">aktywny</span>'}</li>
      <li><span class="detail-key">Ostatnia zmiana</span>dzisiaj, 08:42</li>
      <li><span class="detail-key">Identyfikator</span>stabilny UUID · ukryty w widoku</li>
      ${extraFact}
    </ul>
    <div class="actions">${primaryAction}<button class="action" type="button" data-action="context-edit">Edytuj</button></div>`;
}

function terminalMarkup() {
  const body = state.variant === "A" ? renderVariantA() : state.variant === "B" ? renderVariantB() : renderVariantC();
  return `<div class="terminal terminal-${state.variant.toLowerCase()}">
    <header class="terminal-titlebar"><span>MedAlert</span><span>Linux · baza domyślna · ${new Date().toLocaleTimeString("pl-PL", { hour: "2-digit", minute: "2-digit" })}</span></header>
    <div class="terminal-status"><span class="state-good">● usługa działa</span><span class="state-warn">▲ 1 konto wymaga uwagi</span><span class="state-bad">■ 1 trasa bez sekretu</span><span>${state.notice}</span></div>
    <div class="terminal-body" tabindex="-1">${body}</div>
    ${helpMarkup()}
    <footer class="terminal-help"><span><span class="key">1–5</span> obszary</span><span><span class="key">M</span> sprawdzenie</span><span><span class="key">↑↓</span> wybór</span><span><span class="key">Tab</span> następne pole</span><span><span class="key">Enter</span> wybierz</span><span><span class="key">Esc</span> stan</span><span><span class="key">?</span> pomoc</span></footer>
    <div class="terminal-state"><strong>Pełny stan prototypu:</strong> wariant=${state.variant}; ekran=${state.screen}; obiekt=${state.selectedObject}; ostatnie_działanie=${state.lastAction}</div>
  </div>`;
}

function helpMarkup() {
  if (!state.helpOpen) return "";
  return `<section class="help-panel" role="dialog" aria-modal="true" aria-labelledby="help-title">
    <h2 id="help-title">Pomoc klawiatury</h2>
    <dl><dt>1–5</dt><dd>Otwórz główny obszar.</dd><dt>M</dt><dd>Otwórz ręczne sprawdzenie i monitoring.</dd><dt>↑ / ↓</dt><dd>Przejdź do poprzedniego lub następnego działania.</dd><dt>Tab</dt><dd>Przejdź do następnego pola formularza.</dd><dt>Enter</dt><dd>Wybierz działanie.</dd><dt>Esc</dt><dd>Zamknij pomoc lub wróć do stanu systemu.</dd><dt>?</dt><dd>Otwórz lub zamknij tę pomoc.</dd></dl>
    <div class="actions"><button class="action action-primary" type="button" data-action="help-close" data-focus-id="action:help-close">Zamknij pomoc</button></div>
  </section>`;
}

function currentFocusId() {
  const active = document.activeElement;
  return active instanceof HTMLElement && document.querySelector("#app")?.contains(active) ? active.dataset.focusId ?? null : null;
}

function render() {
  const previousFocus = state.focusAfterRender ?? currentFocusId();
  state.focusAfterRender = null;
  document.querySelector("#app").innerHTML = terminalMarkup();
  const current = variants.find((variant) => variant.key === state.variant);
  document.querySelector("#variant-label").textContent = `${current.key} — ${current.name}`;
  document.querySelector("#announcer").textContent = state.notice;
  bindControls();
  if (previousFocus) {
    document.querySelector(`[data-focus-id="${CSS.escape(previousFocus)}"]`)?.focus();
  }
}

function bindControls() {
  document.querySelectorAll("[data-screen]").forEach((button) => {
    button.addEventListener("click", () => {
      state.screen = button.dataset.screen;
      state.notice = `Otwarto: ${screens[state.screen]?.title ?? "Stan systemu"}`;
      state.lastAction = `otwarcie:${state.screen}`;
      state.focusAfterRender = `screen:${state.screen}`;
      render();
    });
  });

  document.querySelectorAll("[data-object]").forEach((button) => {
    button.addEventListener("click", () => {
      state.selectedObject = button.dataset.object;
      state.notice = "Wybrano obiekt";
      state.lastAction = `wybór:${state.selectedObject}`;
      state.focusAfterRender = `object:${state.selectedObject}`;
      render();
    });
  });

  document.querySelectorAll("[data-action]").forEach((button) => {
    button.addEventListener("click", () => runAction(button.dataset.action));
  });
}

function runAction(action) {
  state.lastAction = action;
  if (action === "authenticate" || (action === "context-action" && state.selectedObject === "account:domowe")) {
    state.screen = "auth";
    state.authStep = "password";
    state.notice = "Uwierzytelnienie: gotowe do rozpoczęcia";
    state.focusAfterRender = "action:auth-next";
  } else if (action === "auth-next") {
    state.authStep = "mfa";
    state.notice = "Uwierzytelnienie: Medicover wymaga kodu MFA";
    state.focusAfterRender = "action:finish-auth";
  } else if (action === "auth-back") {
    state.authStep = "password";
    state.notice = "Uwierzytelnienie: wrócono do hasła";
    state.focusAfterRender = "action:auth-next";
  } else if (action === "finish-auth") {
    state.screen = "accounts";
    state.notice = "Symulacja: konto uwierzytelnione, profile wznowią następny cykl";
    state.focusAfterRender = "screen:accounts";
  } else if (action === "run-check") {
    state.notice = "Symulacja: 2 profile zakończone, 1 czeka na uwierzytelnienie";
  } else if (action === "test-route") {
    state.notice = "Symulacja: wiadomość testowa dostarczona";
  } else if (action === "account-add") {
    state.screen = "accountAdd";
    state.notice = "Dodawanie konta";
  } else if (action === "account-save") {
    state.screen = "auth";
    state.authStep = "password";
    state.notice = "Symulacja: konto zapisane, wymagane uwierzytelnienie";
    state.focusAfterRender = "action:auth-next";
  } else if (action === "profile-add" || action === "profile-edit") {
    state.screen = "profileEdit";
    state.notice = action === "profile-add" ? "Dodawanie profilu na podstawie przykładowych pól" : "Edycja filtrów profilu";
  } else if (action === "profile-save") {
    state.screen = "profiles";
    state.notice = "Symulacja: filtry profilu zapisane";
    state.focusAfterRender = "screen:profiles";
  } else if (action === "profile-disable") {
    state.notice = "Symulacja: profil wyłączony; historia pozostaje";
  } else if (action === "route-edit") {
    state.screen = "routeEdit";
    state.notice = "Naprawa źródła sekretu";
  } else if (action === "route-save") {
    state.screen = "routes";
    state.notice = "Symulacja: sekret zapisany i wiadomość testowa dostarczona";
    state.focusAfterRender = "screen:routes";
  } else if (action === "service-show") {
    state.screen = "service";
    state.notice = "Otwarto stan usługi systemd";
  } else if (action === "service-restart") {
    state.notice = "Symulacja: usługa systemd uruchomiona ponownie";
  } else if (action === "service-log") {
    state.screen = "history";
    state.notice = "Otwarto bezpieczny dziennik usługi";
    state.focusAfterRender = "screen:history";
  } else if (action === "context-edit") {
    if (state.selectedObject.startsWith("profile:")) state.screen = "profileEdit";
    else if (state.selectedObject.startsWith("route:")) state.screen = "routeEdit";
    else if (state.selectedObject.startsWith("account:")) state.screen = "accounts";
    else state.screen = "service";
    state.notice = "Otwarto edycję wybranego obiektu";
  } else if (action === "help-close") {
    state.helpOpen = false;
    state.notice = "Zamknięto pomoc";
  } else {
    state.notice = "Symulacja: działanie zależne od wybranego obiektu";
  }
  render();
}

document.querySelector("#previous-variant").addEventListener("click", () => cycleVariant(-1));
document.querySelector("#next-variant").addEventListener("click", () => cycleVariant(1));

window.addEventListener("keydown", (event) => {
  const target = event.target;
  const acceptsText = target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement || target.isContentEditable;
  if (acceptsText) return;

  const insideTerminal = document.querySelector("#app")?.contains(document.activeElement);

  if ((event.key === "ArrowUp" || event.key === "ArrowDown") && insideTerminal) {
    const controls = [...document.querySelectorAll(".terminal-body button:not([disabled]), .terminal-body input:not([disabled]), .terminal-body select:not([disabled])")];
    if (controls.length > 0) {
      event.preventDefault();
      const current = controls.indexOf(document.activeElement);
      const delta = event.key === "ArrowDown" ? 1 : -1;
      const next = current === -1 ? 0 : (current + delta + controls.length) % controls.length;
      controls[next].focus();
    }
  } else if (event.key === "ArrowLeft" && !insideTerminal) {
    event.preventDefault();
    cycleVariant(-1);
  } else if (event.key === "ArrowRight" && !insideTerminal) {
    event.preventDefault();
    cycleVariant(1);
  } else if (event.key === "Escape") {
    if (state.helpOpen) {
      state.helpOpen = false;
      state.notice = "Zamknięto pomoc";
    } else {
      state.screen = "home";
      state.notice = "Powrót do stanu systemu";
      state.lastAction = "powrót";
    }
    render();
  } else if (event.key.toLowerCase() === "m") {
    state.screen = "monitoring";
    state.notice = "Otwarto: sprawdzenie i monitoring";
    state.lastAction = "skrót:M";
    render();
  } else if (event.key === "?") {
    state.helpOpen = !state.helpOpen;
    state.notice = state.helpOpen ? "Otwarto pomoc klawiatury" : "Zamknięto pomoc";
    state.focusAfterRender = state.helpOpen ? "action:help-close" : null;
    render();
  } else if (["1", "2", "3", "4", "5"].includes(event.key)) {
    const match = menuItems().find(([key]) => key === event.key);
    state.screen = match[1];
    state.notice = `Otwarto: ${match[2]}`;
    state.lastAction = `skrót:${event.key}`;
    state.focusAfterRender = `screen:${state.screen}`;
    render();
  }
});

window.addEventListener("popstate", () => {
  state.variant = readVariant();
  render();
});

render();
