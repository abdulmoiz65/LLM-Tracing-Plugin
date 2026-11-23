// LLM Tracing - built-in Traces page for CLIProxyAPI.
// Security: every value from the API is untrusted. The DOM is built only with
// createElement/textContent; no HTML strings are ever parsed.
(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const RESOURCE_MARK = '/v0/resource/plugins/';
  const SESSION_KEY = 'llm-tracing.management-key';
  const REFRESH_MS = 5000;
  const LIST_LIMIT = 100;
  const SLOW_MS = 10000;
  const SESSION_LIMIT = 20;
  const INSIGHTS_MS = 30000;
  const RANGE_KEY = 'llm-tracing.range';
  const LANGUAGE_KEY = 'llm-tracing.language';
  const RUSSIAN = {
    "last 15 minutes": "последние 15 минут",
    "last hour": "последний час",
    "last 6 hours": "последние 6 часов",
    "last 24 hours": "последние 24 часа",
    "last 7 days": "последние 7 дней",
    "last 30 days": "последние 30 дней",
    "last 90 days": "последние 90 дней",
    "all time": "всё время",
    " ms": " мс",
    " uncached + ": " без кэша + ",
    " cache read + ": " чтение кэша + ",
    " cache write = ": " запись в кэш = ",
    " input tokens": " входных токенов",
    " read": " из кэша",
    " write": " в кэш",
    "unknown": "неизвестно",
    "Canceled before the provider reported usage. It may still have billed the part it generated, so the cost is unknown (not counted).": "Отменено до того, как поставщик сообщил об использовании. Возможно, он все еще выставил счет за сгенерированную часть, поэтому стоимость неизвестна (не учитывается).",
    "Estimated at API list prices": "Оценка по тарифам API",
    ", fixed when the request was recorded": ", исправлено при записи запроса",
    "between ": "между ",
    " and ": " и ",
    ": the proxy does not tell plugins whether cache writes used the 5-minute or 1-hour cache": ": прокси-сервер не сообщает плагинам, используется ли при записи в кэш 5-минутный или 1-часовой кэш.",
    "cache saved ": "экономия за счёт кэша: ",
    " (would be ": " (было бы ",
    "no price for some model; add it under pricing in the plugin config": "для некоторых моделей нет цен. Укажите их в разделе pricing конфигурации плагина",
    " canceled request(s) with unknown cost not included": " отменённых запросов с неизвестной стоимостью исключено",
    " (window unknown)": " (размер окна неизвестен)",
    "Set context_windows in the plugin config for this model.": "Установите context_windows в конфигурации плагина для этой модели.",
    " of ": " из ",
    " context tokens used by the prompt": " токенов контекста занято запросом",
    " - nearly full: start a new conversation or compact": " · Контекст почти заполнен. Начните новый диалог или сожмите историю.",
    " - getting full": " · Контекст заполняется",
    "Failed": "Не удалось",
    "Rejected": "Отклонено",
    "Canceled": "Отменено",
    "Unknown": "Неизвестно",
    " failed)": " с ошибкой)",
    "Enter the management key.": "Введите ключ управления.",
    "Cannot reach the proxy. Check that CLIProxyAPI is running.": "Невозможно связаться с прокси. Убедитесь, что CLIProxyAPI запущен.",
    "Wrong management key — 5 failed attempts lock this IP for 30 minutes.": "Неверный ключ управления. После пяти неудачных попыток IP-адрес блокируется на 30 минут.",
    "Access denied: ": "Доступ запрещен: ",
    "Access denied by the proxy.": "Доступ запрещен прокси.",
    "The LLM Tracing API was not found. Make sure the plugin (v0.2.0 or newer) is enabled.": "LLM Tracing API не найден. Убедитесь, что плагин (версия 0.2.0 или новее) включен.",
    "Request failed (HTTP ": "Запрос не выполнен (HTTP ",
    "The proxy returned an unexpected response.": "Прокси вернул неожиданный ответ.",
    "Refresh failed.": "Не удалось обновить.",
    "Updated ": "Обновлено ",
    " request(s) listed": " запросов в списке",
    " since ": " с ",
    "loading saved history...": "загрузка сохраненной истории...",
    "saved in ": "сохранено в ",
    "save error: ": "ошибка сохранения: ",
    "not saved to disk (save_history: false), lost on restart": "не сохраняется на диск (save_history: false), теряется при перезапуске",
    "attempt details for the newest ": "подробности попыток по последним запросам: ",
    "Cards cover the ": "Сводка за ",
    " request(s) from ": " зап. с ",
    " to ": " до ",
    " (from hourly totals). Sessions, insights and the request list go back to ": " (по почасовым данным). Сеансы, аналитика и список запросов доступны с ",
    "No requests in the ": "Нет запросов за ",
    ". Choose a longer time range.": ". Выберите более длительный временной диапазон.",
    "prices ": "Цены устарели (",
    " days old - update the plugin": " дн.). Обновите плагин.",
    "min ": "от ",
    "saved ": "экономия ",
    "at API list prices": "по тарифам API",
    "What these tokens would cost at API list prices (": "Оценка по тарифам API (",
    "). This is not a bill: subscription (OAuth) ": "). Для подписок с OAuth ",
    "accounts are not billed per token. Each request keeps the price in effect when it was recorded.": "оплата за токены не взимается. Для каждого запроса используется цена на момент записи.",
    " The headline is the upper estimate (1-hour cache writes); \"min\" is the 5-minute price, because the proxy does not tell plugins whether cache writes used the 5-minute or 1-hour cache.": " В заголовке указана верхняя оценка (1 час записи в кэш); «min» — это 5-минутная цена, поскольку прокси-сервер не сообщает плагинам, используется ли при записи в кэш 5-минутный или 1-часовой кэш.",
    " Without the prompt cache: ": " Без кэша запросов: ",
    " The bundled prices are ": " Возраст встроенных цен: ",
    " days old: update the plugin, or set current prices under pricing in the plugin config.": " дн. Обновите плагин или укажите актуальные цены в разделе pricing конфигурации плагина.",
    " No price for: ": " Нет цены на: ",
    " request(s) only partly counted). Add them under pricing in the plugin config.": " зап. учтено частично). Укажите цены в разделе pricing конфигурации плагина.",
    " canceled request(s) have unknown cost and are not included.": " отменённых запросов с неизвестной стоимостью исключено.",
    " failed": " с ошибкой",
    " canceled (not counted)": " отменено (не засчитано)",
    "p95 is approximate for ranges answered from hourly totals.": "p95 является приблизительным для диапазонов, на которые получены ответы по часовым суммам.",
    " output tokens": " выходных токенов",
    " read / ": " из кэша / ",
    " input": " на входе",
    "cache reads / input": "из кэша / входные токены",
    "Local only": "Только локально",
    "No OTLP endpoint configured; traces are only shown here.": "Адрес OTLP не задан. Трассировки доступны только здесь.",
    "Export error": "Ошибка экспорта.",
    "Exporting to ": "Экспорт в ",
    "OTLP endpoint": "адрес OTLP",
    " spans exported, ": " спанов экспортировано, ",
    " dropped": " пропущено",
    "All providers": "Все провайдеры",
    " extra": " доп. расходов",
    "None": "Нет",
    " moved to another account (": " со сменой аккаунта (",
    "; caches are per account)": ", у каждого аккаунта свой кэш)",
    " prompt prefix changed (": " с изменением начала запроса (",
    " after an idle gap (": " после паузы (",
    "Conversations rewrote their cache instead of reading it: ": "Записи в кэш вместо чтения: ",
    "No conversation had to rewrite its cache in this range.": "Ни одному диалогу не пришлось перезаписывать свой кэш в этом диапазоне.",
    "moved to another account": "Смена аккаунта",
    "prefix changed": "Изменение начала запроса",
    "idle ": "Пауза ",
    " rewritten": " токенов заново в кэше",
    ". Click for the request.": ". Нажмите, чтобы открыть запрос.",
    "Re-reading history above ": "Повторное чтение истории сверх ",
    " tokens in ": " токенов, сеансов: ",
    " session(s), ": ", запросов: ",
    " request(s). Compacting or starting fresh earlier avoids most of it.": ". Чтобы сократить расходы, раньше сжимайте историю или начинайте новый диалог.",
    "No request sent more than ": "Ни в одном запросе не превышен порог в ",
    " tokens of context.": " токенов контекста.",
    "Click to show this session's requests.": "Нажмите, чтобы просмотреть запросы этого сеанса.",
    " · peak ": " · максимум ",
    " tokens": " токенов",
    " failed · ": " с ошибкой · ",
    " retried": " с повтором",
    "All OK": "Все запросы успешны",
    "Retries added ": "Дополнительное ожидание из-за повторных попыток: ",
    " of waiting. Failed attempts are usually not billed.": ". Неудачные попытки обычно не оплачиваются.",
    "No request needed a retry. Attempts that failed, per account:": "Ни один запрос не требует повторной попытки. Неудачные попытки по каждому аккаунту:",
    "Every request succeeded on its first attempt.": "Каждый запрос был выполнен с первой попытки.",
    "From the newest ": "Среди последних запросов с подробностями попыток: ",
    " requests with attempt details.": " зап.",
    "vs ": "Вместо ",
    " now (": " по текущей модели (",
    ") for ": ") для ",
    " request(s). Same token counts assumed: tokenizers differ, quality may differ, and switching models mid-conversation rebuilds the cache.": " зап. Расчёт предполагает одинаковое число токенов. Токенизация и качество ответа могут отличаться. При смене модели в ходе диалога кэш создаётся заново.",
    " attempt(s) without a price skipped.": " попыток без цены исключено.",
    "active · ": "активен · ",
    "started ": "начало: ",
    "(none)": "(нет)",
    "Served by ": "Обслужил аккаунт ",
    " canceled": " отменено",
    ". At the current growth, about ": ". При текущем темпе роста поместится ещё около ",
    " more request(s) fit.": " зап.",
    "/req": "/зап.",
    "Prompt grew from ": "Запрос вырос с ",
    " tokens (peak ": " токенов (максимум ",
    " · started ": " · начало: ",
    "Session: ": "Сеанс: ",
    "session request ": "запрос в сеансе: ",
    "Cache hit rate ": "Доля попаданий в кэш: ",
    "No traces match these filters": "По этим фильтрам трассировок нет",
    "No traces yet": "Трассировок пока нет",
    "(no model)": "(нет модели)",
    "stream": "поток",
    "retried": "с повтором",
    "Trace ID": "Идентификатор трассировки",
    "CPA request": "Запрос CPA",
    "Started": "Начало",
    "Duration": "Продолжительность",
    "Client format": "Формат клиента",
    "Attempts": "Попытки",
    "Provider": "Провайдер",
    "Account": "Аккаунт",
    "Tokens in / out": "Токены на входе / выходе",
    "Input breakdown": "Состав входных токенов",
    " uncached · ": " без кэша · ",
    " cache read · ": " чтение кэша · ",
    " cache write": " запись в кэш",
    "Cache hit rate": "Доля попаданий в кэш",
    "Est. API cost": "Расчётная стоимость API",
    "unknown (canceled before usage was reported)": "неизвестно (отменено до того, как было сообщено об использовании)",
    " (without cache ": " (без кэша ",
    "Price source": "Источник цены",
    ", fixed when recorded": ", фиксируется при записи",
    "Context used": "Используемый контекст",
    "Session": "Сеанс",
    "request ": "запрос ",
    "Client key": "Ключ клиента",
    "Attempt details are kept only for the newest requests. This older request keeps its totals, cost and session.": "Сведения о попытках сохраняются только для самых новых запросов. Этот более старый запрос сохраняет итоговые данные, стоимость и сеанс.",
    " more attempt(s) are counted but not kept in memory.": " дополнительные попытки засчитываются, но не сохраняются в памяти.",
    "Request": "Запрос",
    " - failed": " — не удалось",
    " at ": " в ",
    " after attempt start": " от начала попытки",
    "No upstream attempt was recorded (for example, the request was rejected before reaching a provider).": "Попыток обращения к провайдеру не записано. Возможно, запрос отклонён раньше.",
    "Attempt ": "Попытка ",
    "failed": "не удалось",
    "Model": "Модель",
    "Response model": "Модель ответа",
    "Status code": "Код состояния",
    "Latency": "Задержка",
    "Input (total)": "Ввод (всего)",
    "Uncached input": "Некэшированный ввод",
    "Cache read": "Чтение кэша",
    "Cache write": "Запись в кэш",
    "Output": "Выход",
    "Reasoning": "Рассуждение",
    "Est. cost": "Расчётная стоимость",
    " (fixed when recorded)": " (фиксируется при записи)",
    "Auth type": "Тип аутентификации",
    "Auth index": "Индекс авторизации",
    "Error": "Ошибка",
    "All attributes": "Все атрибуты",
    "Copied": "Скопировано",
    "Copy failed": "Не удалось скопировать",
    "Copy trace ID": "Копировать идентификатор трассировки",
    "Disconnect": "Отключиться",
    "Connect to this proxy": "Подключение к прокси",
    "Enter the CPA management key. It is kept in this page's memory and sent only to this proxy, never in the URL.": "Введите ключ управления CPA. Ключ хранится в памяти страницы и передаётся только этому прокси, в заголовке запроса.",
    "Management key": "Ключ управления",
    "Connect": "Подключиться",
    "Remember for this tab": "Запомнить в этой вкладке",
    "Time range": "Временной диапазон",
    "Last 15 minutes": "Последние 15 минут",
    "Last hour": "Последний час",
    "Last 6 hours": "Последние 6 часов",
    "Last 24 hours": "Последние 24 часа",
    "Last 7 days": "Последние 7 дней",
    "Last 30 days": "Последние 30 дней",
    "Last 90 days": "Последние 90 дней",
    "All time": "За всё время",
    "Summary for the selected time range": "Сводная информация за выбранный временной диапазон.",
    "Requests": "Запросы",
    "Error rate": "Частота ошибок",
    "Avg / p95 latency": "Средняя задержка / p95",
    "end to end": "за весь запрос",
    "input incl. cache, all attempts": "вход вкл. кэш, все попытки",
    "Retried requests": "Повторные запросы",
    "more than one attempt": "более одной попытки",
    "Insights": "Информация",
    "Estimated from recorded token counts at API list prices, for the selected time range.": "Рассчитано на основе зарегистрированного количества токенов по прейскурантным ценам API для выбранного временного диапазона.",
    "Cache rewrites": "Перезапись кэша",
    "What is this?": "Что это?",
    "Coding agents resend the whole conversation every turn. Normally it is read from the prompt cache, which is cheap (Opus 5.5: $0.20 per million tokens). When the cache is gone, the whole conversation is written again at the much higher write price ($5 to $8 per million). The amount is that extra cost. Causes: the conversation moved to another account (each account has its own cache), the start of the prompt changed, or the cache expired during a pause.": "Агенты кодирования каждый раз пересылают весь разговор. Обычно он считывается из кэша подсказок, что обходится дешево (Opus 5.5: 0,20 доллара США за миллион токенов). Когда кэш исчезает, весь диалог записывается заново с гораздо более высокой ценой записи (от 5 до 8 долларов за миллион). Сумма и есть дополнительные расходы. Причины: разговор переместился в другой аккаунт (у каждого аккаунта свой кэш), изменилось начало подсказки или истек срок действия кэша во время паузы.",
    "Large context": "Большой контекст",
    "Every turn re-reads the whole conversation history. Once it is past 200K tokens, each turn pays to read all of it again, even from the cache. The amount is what reading the part above 200K cost. Compacting the conversation or starting a new one earlier avoids most of it.": "Каждый ход перечитывает всю историю разговора. Как только количество токенов превысит 200 000, каждый ход стоит прочитать все это снова, даже из кэша. Сумма равна стоимости прочтения части выше 200 тысяч. Сжатие разговора или начало нового раньше позволяет избежать большей части его.",
    "Failures & retries": "Неудачи и повторные попытки",
    "When an attempt fails (for example a 429 rate limit or a server error) the proxy can try again, often on another account. Failed attempts are usually not billed, so the cost of a retry is the extra waiting time. Failures that were not retried are listed per account.": "Если попытка не удалась (например, ограничение скорости 429 или ошибка сервера), прокси-сервер может повторить попытку, часто с другой учетной записью. Неудачные попытки обычно не оплачиваются, поэтому стоимость повторной попытки — это дополнительное время ожидания. Ошибки, которые не были повторены, перечислены для каждой учетной записи.",
    "What if": "Что, если",
    "Your recorded token counts priced on another model. It is a rough comparison: models count tokens differently, answer quality differs, and switching models in the middle of a conversation rebuilds the cache.": "Количество ваших записанных токенов рассчитано на другую модель. Это грубое сравнение: модели по-разному считают токены, разное качество ответов, а переключение моделей в середине разговора перестраивает кэш.",
    "Same tokens on": "Те же токены на",
    "Sessions": "Сеансы",
    "Estimated: requests from the same client key and model whose prompt keeps growing. Click one to see its requests.": "Расчетное: запросы от одного и того же ключа клиента и модели, количество запросов которых продолжает расти. Нажмите один, чтобы увидеть его запросы.",
    "No sessions in this time range.": "За этот период сеансов нет.",
    "Last activity": "Последняя активность",
    "Prompt size of the latest request against the model's context window": "Подскажите размер последнего запроса в контекстном окне модели.",
    "Average prompt growth per request": "Средний быстрый рост на запрос",
    "Growth": "Рост",
    "Show all sessions": "Показать все сессии",
    "Show all": "Показать все",
    "Status": "Статус",
    "All": "Все",
    "Errors": "Ошибки",
    "Filter by model": "Фильтровать по модели",
    "Slow only (>10s)": "Только медленные (>10 с)",
    "Auto-refresh (5s)": "Автообновление (5 с)",
    "Refresh": "Обновить",
    "Traces appear here a few seconds after requests go through the proxy.": "Здесь появляются следы через несколько секунд после прохождения запросов через прокси.",
    "Try a longer time range if you expected requests here.": "Попробуйте более длительный диапазон времени, если вы ожидаете запросов здесь.",
    "Time": "Время",
    "Credential that served the request (last successful attempt)": "Учетные данные, обслужившие запрос (последняя успешная попытка)",
    "Total input (uncached + cache read + cache write) / output, all attempts": "Общий ввод (некэшированный + чтение кэша + запись в кэш)/выход, все попытки",
    "Cache": "Тайник",
    "Prompt size against the model's context window": "Размер подсказки относительно контекстного окна модели.",
    "Context": "Контекст",
    "Close trace details": "Закрыть детали трассировки",
    "Close": "Закрыть",
    "Attempt timeline": "График попыток",
    " s": " с",
    "m ": " мин ",
    "s": " с",
    "k": " тыс.",
    "M": " млн",
    "Language": "Язык"
  };
  let language;

  function translateMessage(message) {
    return language === 'ru' && Object.hasOwn(RUSSIAN, message) ? RUSSIAN[message] : message;
  }

  function initLanguage() {
    let saved;
    try { saved = storage()?.getItem(LANGUAGE_KEY); } catch {}
    const browserLanguage = navigator.language.toLowerCase().split('-')[0];
    const select = $('language');
    const bindings = [...document.querySelectorAll('[data-i18n]')].map(node => {
      const message = node.textContent;
      return () => { node.textContent = translateMessage(message); };
    });
    for (const name of ['title', 'aria-label', 'placeholder']) {
      for (const node of document.querySelectorAll('[data-i18n-' + name + ']')) {
        const message = node.getAttribute(name);
        bindings.push(() => node.setAttribute(name, translateMessage(message)));
      }
    }
    function apply(next) {
      language = next;
      document.documentElement.lang = next;
      select.value = next;
      for (const bind of bindings) bind();
    }

    apply(saved === 'ru' || saved === 'en' ? saved : browserLanguage === 'ru' ? 'ru' : 'en');
    select.addEventListener('change', () => {
      apply(select.value);
      try { storage()?.setItem(LANGUAGE_KEY, language); } catch {}
      renderSessionChip();
      showConnectError('');
      insightsAt = 0;
      if (managementKey) refresh(false);
    });
  }

  const RANGE_LABELS = { '15m': 'last 15 minutes', '1h': 'last hour', '6h': 'last 6 hours', '24h': 'last 24 hours', '7d': 'last 7 days', '30d': 'last 30 days', '90d': 'last 90 days', all: 'all time' };

  // The page is served at <prefix>/v0/resource/plugins/<id>/dashboard; the
  // API lives on the same origin under <prefix>/v0/management/.
  const apiBase = (() => {
    const path = location.pathname;
    const at = path.indexOf(RESOURCE_MARK);
    const prefix = at > 0 ? path.slice(0, at) : '';
    return prefix + '/v0/management/plugins/llm-tracing';
  })();

  let managementKey = '';
  let timer = null;
  let busy = false;
  let selectedId = '';
  let modelDebounce = null;
  let insightsAt = 0;
  let insightsRange = '';
  let sessionFilter = null; // Selected session when the table is filtered

  // ---------- small DOM helpers ----------
  function el(tag, text, className) {
    const node = document.createElement(tag);
    if (text !== undefined && text !== null) node.textContent = String(text);
    if (className) node.className = className;
    return node;
  }
  function str(value) {
    return value === undefined || value === null ? '' : String(value);
  }
  function num(value) {
    const n = Number(value);
    return Number.isFinite(n) ? n : 0;
  }

  // ---------- formatting ----------
  function fmtDuration(ms) {
    ms = num(ms);
    if (ms < 1000) return Math.round(ms) + translateMessage(' ms');
    if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + translateMessage(' s');
    const minutes = Math.floor(ms / 60000);
    return minutes + translateMessage('m ') + Math.round((ms % 60000) / 1000) + translateMessage('s');
  }
  function fmtInt(value) {
    return Math.round(num(value)).toLocaleString(language === 'ru' ? 'ru' : undefined);
  }
  // fmtCompact renders 950, 12.3k, 1.2M.
  function fmtCompact(value) {
    const n = Math.round(num(value));
    const abs = Math.abs(n);
    if (abs < 1000) return String(n);
    if (abs < 1e6) return (n / 1e3).toFixed(abs < 1e4 ? 1 : 0).replace(/\.0$/, '') + translateMessage('k');
    return (n / 1e6).toFixed(abs < 1e7 ? 1 : 0).replace(/\.0$/, '') + translateMessage('M');
  }
  // Token objects (trace rows, summary, per-attempt "tokens") share the
  // field names total_input_tokens, cache_read_tokens, cache_creation_tokens.
  function uncachedOf(t) {
    return Math.max(0, num(t.total_input_tokens) - num(t.cache_read_tokens) - num(t.cache_creation_tokens));
  }
  function inputBreakdown(t) {
    return fmtInt(uncachedOf(t)) + translateMessage(' uncached + ') + fmtInt(t.cache_read_tokens) + translateMessage(' cache read + ') +
      fmtInt(t.cache_creation_tokens) + translateMessage(' cache write = ') + fmtInt(t.total_input_tokens) + translateMessage(' input tokens');
  }
  function cacheText(t) {
    const read = num(t.cache_read_tokens);
    const write = num(t.cache_creation_tokens);
    const parts = [];
    if (read > 0) parts.push(fmtCompact(read) + translateMessage(' read'));
    if (write > 0) parts.push(fmtCompact(write) + translateMessage(' write'));
    return parts.length ? parts.join(' · ') : '-';
  }
  function fmtTime(iso) {
    const d = new Date(str(iso));
    if (Number.isNaN(d.getTime())) return '-';
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    return sameDay ? d.toLocaleTimeString(language === 'ru' ? 'ru' : undefined) : d.toLocaleString(language === 'ru' ? 'ru' : undefined);
  }
  // fmtPercent keeps one decimal near the ends so 99.7% never shows as 100%.
  function fmtPercent(rate) {
    const r = num(rate);
    const decimals = (r > 0 && r < 0.1) || (r > 0.99 && r < 1) ? 1 : 0;
    let text = (r * 100).toFixed(decimals);
    if (r < 1 && text === '100.0') text = '99.9';
    return text + '%';
  }
  // fmtUSD renders $0.0042, $0.42, $12.34, $1.2k.
  function fmtUSD(value) {
    const n = num(value);
    if (n === 0) return '$0';
    if (n < 0.01) return '$' + n.toFixed(4);
    if (n < 1000) return '$' + n.toFixed(2);
    return '$' + fmtCompact(n);
  }
  // hasRange: the cache write type was guessed, so the cost is a range.
  function hasRange(t) {
    return num(t.cost_min_usd) > 0 && num(t.cost_min_usd) < num(t.cost_usd) * 0.995;
  }
  // Row and session costs show the upper value; '+' marks a partial total.
  function costText(t) {
    if (t.cost_unknown) return translateMessage('unknown');
    const partial = t.priced === false || num(t.unknown_cost_requests) > 0;
    if (t.priced === false && num(t.cost_usd) === 0) return '-';
    return fmtUSD(t.cost_usd) + (partial ? '*' : '');
  }
  function costTitle(t) {
    if (t.cost_unknown) {
      return translateMessage('Canceled before the provider reported usage. It may still have billed the part it generated, so the cost is unknown (not counted).');
    }
    const parts = [translateMessage('Estimated at API list prices') + (str(t.price_source) ? ' (' + str(t.price_source) + ')' : '') +
      translateMessage(', fixed when the request was recorded')];
    if (hasRange(t)) {
      parts.push(translateMessage('between ') + fmtUSD(t.cost_min_usd) + translateMessage(' and ') + fmtUSD(t.cost_usd) +
        translateMessage(': the proxy does not tell plugins whether cache writes used the 5-minute or 1-hour cache'));
    }
    const saved = num(t.cost_without_cache_usd) - num(t.cost_usd);
    if (saved > 0) parts.push(translateMessage('cache saved ') + fmtUSD(saved) + translateMessage(' (would be ') + fmtUSD(t.cost_without_cache_usd) + ')');
    if (t.priced === false) parts.push(translateMessage('no price for some model; add it under pricing in the plugin config'));
    if (num(t.unknown_cost_requests) > 0) parts.push(fmtInt(t.unknown_cost_requests) + translateMessage(' canceled request(s) with unknown cost not included'));
    return parts.join('; ');
  }
  // ctxGauge renders a context-window gauge; warn at 70%, bad at 90%.
  function ctxGauge(tokens, windowSize, used) {
    const box = el('div', undefined, 'ctx');
    tokens = num(tokens);
    windowSize = num(windowSize);
    if (tokens <= 0) {
      box.append(el('span', '-', 'ctx-text'));
      return box;
    }
    if (windowSize <= 0) {
      box.append(el('span', fmtCompact(tokens) + translateMessage(' (window unknown)'), 'ctx-text'));
      box.title = translateMessage('Set context_windows in the plugin config for this model.');
      return box;
    }
    const ratio = Math.min(1, Math.max(0, num(used)));
    if (ratio >= 0.9) box.classList.add('bad');
    else if (ratio >= 0.7) box.classList.add('warn');
    const track = el('div', undefined, 'ctx-track');
    const fill = el('div', undefined, 'ctx-fill');
    fill.style.width = Math.max(ratio * 100, 1) + '%';
    track.append(fill);
    box.append(track, el('span', fmtCompact(tokens) + ' / ' + fmtCompact(windowSize) + ' · ' + fmtPercent(ratio), 'ctx-text'));
    box.title = fmtInt(tokens) + translateMessage(' of ') + fmtInt(windowSize) + translateMessage(' context tokens used by the prompt') +
      (ratio >= 0.9 ? translateMessage(' - nearly full: start a new conversation or compact') : ratio >= 0.7 ? translateMessage(' - getting full') : '');
    return box;
  }
  function currentRange() {
    return $('f-range').value || 'all';
  }

  function isError(t) {
    return str(t.outcome) !== 'canceled' && (str(t.outcome) !== 'succeeded' || num(t.status_code) >= 400);
  }
  function statusBadge(t) {
    const outcome = str(t.outcome) || 'unknown';
    const code = num(t.status_code);
    const labels = { succeeded: 'OK', failed: translateMessage('Failed'), rejected: translateMessage('Rejected'), canceled: translateMessage('Canceled'), unknown: translateMessage('Unknown') };
    let label = labels[outcome] || outcome;
    if (code > 0) label += ' ' + code;
    let kind = 'bad';
    if (!isError(t)) kind = 'ok';
    else if (outcome === 'rejected' || outcome === 'canceled') kind = 'warn';
    else if (outcome === 'unknown') kind = '';
    return el('span', label, 'badge' + (kind ? ' ' + kind : ''));
  }
  function attemptsText(t) {
    const count = num(t.attempt_count);
    const failed = num(t.failed_attempts);
    return failed > 0 ? count + ' (' + failed + translateMessage(' failed)') : String(count);
  }

  // ---------- API ----------
  class ApiError extends Error {
    constructor(message, status) {
      super(message);
      this.status = status;
    }
  }

  async function api(path) {
    if (!managementKey) throw new ApiError(translateMessage('Enter the management key.'), 401);
    const controller = new AbortController();
    const abort = setTimeout(() => controller.abort(), 15000);
    let response;
    try {
      response = await fetch(apiBase + path, {
        method: 'GET',
        headers: { Authorization: 'Bearer ' + managementKey, Accept: 'application/json' },
        credentials: 'omit',
        cache: 'no-store',
        referrerPolicy: 'no-referrer',
        signal: controller.signal,
      });
    } catch (err) {
      throw new ApiError(translateMessage('Cannot reach the proxy. Check that CLIProxyAPI is running.'), 0);
    } finally {
      clearTimeout(abort);
    }
    let body = null;
    try {
      body = await response.json();
    } catch (err) {
      body = null;
    }
    if (!response.ok) {
      const serverMessage = body && typeof body.error === 'string' ? body.error : '';
      if (response.status === 401) {
        throw new ApiError(translateMessage('Wrong management key — 5 failed attempts lock this IP for 30 minutes.'), 401);
      }
      if (response.status === 403) {
        throw new ApiError(serverMessage ? translateMessage('Access denied: ') + serverMessage : translateMessage('Access denied by the proxy.'), 403);
      }
      if (response.status === 404 && !serverMessage) {
        throw new ApiError(translateMessage('The LLM Tracing API was not found. Make sure the plugin (v0.2.0 or newer) is enabled.'), 404);
      }
      throw new ApiError(serverMessage || translateMessage('Request failed (HTTP ') + response.status + ').', response.status);
    }
    if (!body || typeof body !== 'object') throw new ApiError(translateMessage('The proxy returned an unexpected response.'), response.status);
    return body;
  }

  // ---------- connect / disconnect ----------
  function storage() {
    try {
      return window.sessionStorage;
    } catch (err) {
      return null;
    }
  }

  function showConnectError(message) {
    const node = $('connect-error');
    node.textContent = message;
    node.hidden = !message;
  }

  async function connect() {
    const key = $('key').value.trim();
    if (!key) {
      showConnectError(translateMessage('Enter the management key.'));
      return;
    }
    managementKey = key;
    $('connect-btn').disabled = true;
    showConnectError('');
    try {
      await refresh(true);
      const store = storage();
      if (store) {
        if ($('remember').checked) store.setItem(SESSION_KEY, key);
        else store.removeItem(SESSION_KEY);
      }
      $('key').value = '';
      $('connect').hidden = true;
      $('app').hidden = false;
      $('connbar').hidden = false;
      schedule();
    } catch (err) {
      managementKey = '';
      showConnectError(err.message);
    } finally {
      $('connect-btn').disabled = false;
    }
  }

  function disconnect(message) {
    managementKey = '';
    stopTimer();
    const store = storage();
    if (store) store.removeItem(SESSION_KEY);
    selectedId = '';
    sessionFilter = null;
    renderSessionChip();
    $('rows').replaceChildren();
    $('session-rows').replaceChildren();
    $('detail').hidden = true;
    $('app').hidden = true;
    $('connbar').hidden = true;
    $('connect').hidden = false;
    showConnectError(message || '');
    $('key').focus();
  }

  // ---------- refresh loop ----------
  function stopTimer() {
    if (timer) clearInterval(timer);
    timer = null;
  }

  function schedule() {
    stopTimer();
    if (!managementKey || !$('f-auto').checked || document.visibilityState !== 'visible') return;
    timer = setInterval(() => {
      if (document.visibilityState === 'visible') refresh(false).catch(handleError);
    }, REFRESH_MS);
  }

  function handleError(err) {
    if (err && (err.status === 401 || err.status === 403)) {
      // Stop retrying: CPA bans an IP after repeated failed key attempts.
      disconnect(err.message);
      return;
    }
    $('status-line').textContent = err && err.message ? err.message : translateMessage('Refresh failed.');
  }

  function listQuery() {
    const params = new URLSearchParams();
    params.set('limit', String(LIST_LIMIT));
    params.set('status', $('f-status').value);
    const model = $('f-model').value.trim();
    if (model) params.set('model', model.slice(0, 200));
    const provider = $('f-provider').value;
    if (provider) params.set('provider', provider.slice(0, 200));
    if ($('f-slow').checked) params.set('min_ms', String(SLOW_MS));
    params.set('range', currentRange());
    if (sessionFilter) params.set('session', sessionFilter.id);
    return '/traces?' + params.toString();
  }

  async function refresh(throwErrors) {
    if (busy) return;
    busy = true;
    const languageAtStart = language;
    try {
      const range = encodeURIComponent(currentRange());
      const [summary, list, sessions] = await Promise.all([
        api('/summary?range=' + range),
        api(listQuery()),
        api('/sessions?range=' + range + '&limit=' + SESSION_LIMIT),
      ]);
      renderSummary(summary);
      renderSessions(Array.isArray(sessions.sessions) ? sessions.sessions : []);
      renderTraces(Array.isArray(list.traces) ? list.traces : []);
      // Insights scan the whole history: refresh them every 30 s, not 5 s.
      if (Date.now() - insightsAt > INSIGHTS_MS || insightsRange !== currentRange()) {
        insightsAt = Date.now();
        insightsRange = currentRange();
        loadInsights().catch(handleError);
      }
      if (selectedId) await loadDetail(selectedId, false);
      $('status-line').textContent = translateMessage('Updated ') + new Date().toLocaleTimeString(language === 'ru' ? 'ru' : undefined) + ' - ' + historyText(summary);
    } catch (err) {
      if (throwErrors) throw err;
      handleError(err);
    } finally {
      busy = false;
      if (!throwErrors && languageAtStart !== language && managementKey) refresh(false);
    }
  }

  function historyText(s) {
    const h = s.history && typeof s.history === 'object' ? s.history : {};
    const parts = [fmtInt(s.stored_traces) + translateMessage(' request(s) listed') + (str(s.detail_since) ? translateMessage(' since ') + fmtTime(s.detail_since) : '')];
    if (h.enabled) {
      parts.push(h.loading ? translateMessage('loading saved history...') : translateMessage('saved in ') + str(h.dir));
      if (str(h.last_error)) parts.push(translateMessage('save error: ') + str(h.last_error));
    } else {
      parts.push(translateMessage('not saved to disk (save_history: false), lost on restart'));
    }
    parts.push(translateMessage('attempt details for the newest ') + fmtInt(s.detail_traces));
    return parts.join(' · ');
  }

  // ---------- rendering ----------
  function renderSummary(s) {
    $('version').textContent = s.version ? 'v' + str(s.version) : '';
    $('m-requests').textContent = fmtInt(s.requests);
    const rangeLabel = translateMessage(RANGE_LABELS[str(s.range)] || RANGE_LABELS[currentRange()] || '');
    $('m-requests-hint').textContent = rangeLabel;
    const line = $('range-line');
    const hourly = str(s.source) === 'hourly';
    if (num(s.requests) > 0 && str(s.oldest_trace_at)) {
      line.textContent = translateMessage('Cards cover the ') + rangeLabel + ': ' + fmtInt(s.requests) + translateMessage(' request(s) from ') +
        fmtTime(s.oldest_trace_at) + translateMessage(' to ') + fmtTime(s.newest_trace_at) +
        (hourly ? translateMessage(' (from hourly totals). Sessions, insights and the request list go back to ') + fmtTime(s.detail_since) +
          ' (history_days / history_max_requests).' : '.');
    } else if (num(s.stored_traces) > 0) {
      line.textContent = translateMessage('No requests in the ') + rangeLabel + translateMessage('. Choose a longer time range.');
    } else {
      line.textContent = '';
    }
    const h = s.history && typeof s.history === 'object' ? s.history : {};
    const warn = $('history-warning');
    warn.textContent = str(h.warning);
    warn.hidden = !str(h.warning);
    const cost = $('m-cost');
    // One headline number (the upper estimate); details go in the hint line.
    cost.textContent = num(s.requests) === 0 ? '-' : fmtUSD(s.cost_usd);
    const savings = num(s.cache_savings_usd);
    const costHint = $('m-cost-hint');
    costHint.classList.remove('ok', 'warn');
    if (s.prices_stale) {
      costHint.textContent = translateMessage('prices ') + fmtInt(s.prices_age_days) + translateMessage(' days old - update the plugin');
      costHint.classList.add('warn');
    } else {
      const parts = [];
      if (hasRange(s)) parts.push(el('span', translateMessage('min ') + fmtUSD(s.cost_min_usd)));
      if (savings > 0) parts.push(el('span', translateMessage('saved ') + fmtUSD(savings), 'ok-text'));
      if (parts.length === 0) parts.push(el('span', translateMessage('at API list prices')));
      const nodes = [];
      parts.forEach((node, i) => {
        if (i > 0) nodes.push(' · ');
        nodes.push(node);
      });
      costHint.replaceChildren(...nodes);
    }
    const unpriced = Array.isArray(s.unpriced_models) ? s.unpriced_models.map(str).filter((m) => m) : [];
    cost.title = translateMessage('What these tokens would cost at API list prices (') + str(s.prices_as_of) + translateMessage('). This is not a bill: subscription (OAuth) ') +
      translateMessage('accounts are not billed per token. Each request keeps the price in effect when it was recorded.') +
      (hasRange(s) ? translateMessage(' The headline is the upper estimate (1-hour cache writes); "min" is the 5-minute price, because the proxy does not tell plugins whether cache writes used the 5-minute or 1-hour cache.') : '') +
      translateMessage(' Without the prompt cache: ') + fmtUSD(s.cost_without_cache_usd) + '.' +
      (s.prices_stale ? translateMessage(' The bundled prices are ') + fmtInt(s.prices_age_days) + translateMessage(' days old: update the plugin, or set current prices under pricing in the plugin config.') : '') +
      (unpriced.length ? translateMessage(' No price for: ') + unpriced.join(', ') + ' (' + fmtInt(s.unpriced_requests) + translateMessage(' request(s) only partly counted). Add them under pricing in the plugin config.') : '') +
      (num(s.unknown_cost_requests) > 0 ? ' ' + fmtInt(s.unknown_cost_requests) + translateMessage(' canceled request(s) have unknown cost and are not included.') : '');
    $('m-errors').textContent = fmtPercent(s.error_rate);
    $('m-errors-hint').textContent = fmtInt(s.errors) + translateMessage(' failed') + (num(s.canceled) > 0 ? ' · ' + fmtInt(s.canceled) + translateMessage(' canceled (not counted)') : '');
    $('m-latency').textContent = fmtDuration(s.avg_latency_ms) + ' / ' + (s.p95_approximate ? '≈' : '') + fmtDuration(s.p95_latency_ms);
    $('m-latency').title = s.p95_approximate ? translateMessage('p95 is approximate for ranges answered from hourly totals.') : '';
    $('m-tokens').textContent = fmtCompact(s.total_input_tokens) + ' / ' + fmtCompact(s.output_tokens);
    $('m-tokens').title = inputBreakdown(s) + '; ' + fmtInt(s.output_tokens) + translateMessage(' output tokens');
    $('m-cache').textContent = num(s.total_input_tokens) > 0 ? fmtPercent(s.cache_hit_rate) : '-';
    $('m-cache-hint').textContent = num(s.total_input_tokens) > 0
      ? fmtCompact(s.cache_read_tokens) + translateMessage(' read / ') + fmtCompact(s.total_input_tokens) + translateMessage(' input')
      : translateMessage('cache reads / input');
    $('m-retried').textContent = fmtInt(s.retried_requests);
    renderProviders(Array.isArray(s.providers) ? s.providers : []);

    const pill = $('exporter');
    const exp = s.exporter && typeof s.exporter === 'object' ? s.exporter : {};
    pill.hidden = false;
    pill.title = '';
    if (!exp.enabled) {
      pill.textContent = translateMessage('Local only');
      pill.className = 'pill';
      pill.title = translateMessage('No OTLP endpoint configured; traces are only shown here.');
    } else if (str(exp.last_error)) {
      pill.textContent = translateMessage('Export error');
      pill.className = 'pill bad';
      pill.title = str(exp.last_error);
    } else {
      pill.textContent = translateMessage('Exporting to ') + (str(exp.endpoint_host) || translateMessage('OTLP endpoint'));
      pill.className = 'pill ok';
      pill.title = fmtInt(exp.exported_spans) + translateMessage(' spans exported, ') + fmtInt(exp.dropped_spans) + translateMessage(' dropped');
    }
  }

  function renderProviders(providers) {
    const select = $('f-provider');
    const current = select.value;
    const fragment = document.createDocumentFragment();
    const all = el('option', translateMessage('All providers'));
    all.value = '';
    fragment.append(all);
    const names = providers.map(str).filter((p) => p);
    if (current && !names.includes(current)) names.push(current);
    // Rebuilding on every refresh would close an open dropdown.
    const signature = names.join('\n');
    if (select.dataset.signature === signature) return;
    select.dataset.signature = signature;
    for (const name of names) {
      const option = el('option', name);
      option.value = name;
      fragment.append(option);
    }
    select.replaceChildren(fragment);
    select.value = current;
  }

  // ---------- insights ----------
  async function loadInsights() {
    const range = encodeURIComponent(currentRange());
    const model = encodeURIComponent($('i-whatif-model').value);
    const [ins, what] = await Promise.all([
      api('/insights?range=' + range),
      api('/whatif?range=' + range + '&model=' + model).catch((err) => ({ error: err.message })),
    ]);
    renderInsights(ins);
    renderWhatIf(what);
  }

  // item adds a list row; with sub, the left side has a second, muted line.
  function item(list, left, right, onClick, title, sub) {
    const li = el('li');
    if (sub) {
      const two = el('div', undefined, 'two');
      two.append(el('span', left), el('span', sub, 'sub'));
      li.append(two, el('span', right));
    } else {
      li.append(el('span', left), el('span', right));
    }
    if (title) li.title = title;
    if (onClick) {
      li.className = 'link';
      li.tabIndex = 0;
      li.addEventListener('click', onClick);
      li.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          onClick();
        }
      });
    }
    list.append(li);
  }

  function renderInsights(d) {
    const b = d.cache_breaks && typeof d.cache_breaks === 'object' ? d.cache_breaks : {};
    const moved = num(b.account_changed);
    const changed = num(b.prefix_changed);
    const unknown = num(b.expired_or_changed);
    const breakUSD = num(b.account_changed_usd) + num(b.prefix_changed_usd) + num(b.expired_or_changed_usd);
    $('i-breaks').textContent = moved + changed + unknown > 0 ? '+' + fmtUSD(breakUSD) + translateMessage(' extra') : translateMessage('None');
    const why = [];
    if (moved > 0) why.push(fmtInt(moved) + translateMessage(' moved to another account (') + fmtUSD(b.account_changed_usd) + translateMessage('; caches are per account)'));
    if (changed > 0) why.push(fmtInt(changed) + translateMessage(' prompt prefix changed (') + fmtUSD(b.prefix_changed_usd) + ')');
    if (unknown > 0) why.push(fmtInt(unknown) + translateMessage(' after an idle gap (') + fmtUSD(b.expired_or_changed_usd) + ')');
    $('i-breaks-text').textContent = why.length
      ? translateMessage('Conversations rewrote their cache instead of reading it: ') + why.join(', ') + '.'
      : translateMessage('No conversation had to rewrite its cache in this range.');
    const breaks = $('i-breaks-list');
    breaks.replaceChildren();
    for (const x of Array.isArray(b.top) ? b.top : []) {
      const kinds = { account_changed: translateMessage('moved to another account'), prefix_changed: translateMessage('prefix changed') };
      const kind = kinds[str(x.kind)] || translateMessage('idle ') + fmtDuration(num(x.gap_seconds) * 1000);
      item(breaks, fmtTime(x.at) + ' · ' + fmtCompact(x.write_tokens) + translateMessage(' rewritten'), '+' + fmtUSD(x.extra_usd) + translateMessage(' extra'),
        () => select(str(x.trace_id)), str(x.model) + ' · ' + str(x.account) + translateMessage('. Click for the request.'), kind + ' · ' + str(x.account));
    }

    const c = d.context && typeof d.context === 'object' ? d.context : {};
    $('i-context').textContent = num(c.sessions) > 0 ? fmtUSD(c.above_threshold_usd) : translateMessage('None');
    $('i-context-text').textContent = num(c.sessions) > 0
      ? translateMessage('Re-reading history above ') + fmtCompact(c.threshold_tokens) + translateMessage(' tokens in ') + fmtInt(c.sessions) +
        translateMessage(' session(s), ') + fmtInt(c.requests) + translateMessage(' request(s). Compacting or starting fresh earlier avoids most of it.')
      : translateMessage('No request sent more than ') + fmtCompact(c.threshold_tokens) + translateMessage(' tokens of context.');
    const ctx = $('i-context-list');
    ctx.replaceChildren();
    for (const x of Array.isArray(c.top) ? c.top : []) {
      const session = { id: str(x.session_id), model: str(x.model), account: str(x.account) };
      item(ctx, str(x.account), fmtUSD(x.above_threshold_usd),
        () => toggleSession(session), translateMessage('Click to show this session\'s requests.'),
        str(x.model) + translateMessage(' · peak ') + fmtCompact(x.peak_tokens) + translateMessage(' tokens'));
    }

    const r = d.retries && typeof d.retries === 'object' ? d.retries : {};
    const accounts = Array.isArray(r.accounts) ? r.accounts : [];
    const failedTotal = accounts.reduce((sum, x) => sum + num(x.failed), 0);
    const retried = num(r.retried_requests);
    $('i-retries').textContent = failedTotal + retried > 0
      ? fmtInt(failedTotal) + translateMessage(' failed · ') + fmtInt(retried) + translateMessage(' retried')
      : translateMessage('All OK');
    $('i-retries-text').textContent = retried > 0
      ? translateMessage('Retries added ') + fmtDuration(r.added_wait_ms) + translateMessage(' of waiting. Failed attempts are usually not billed.')
      : (failedTotal > 0 ? translateMessage('No request needed a retry. Attempts that failed, per account:') : translateMessage('Every request succeeded on its first attempt.'));
    const acc = $('i-retries-list');
    acc.replaceChildren();
    for (const x of Array.isArray(r.accounts) ? r.accounts : []) {
      if (num(x.failed) === 0) continue;
      item(acc, str(x.account), fmtInt(x.failed) + translateMessage(' of ') + fmtInt(x.attempts) + translateMessage(' failed'),
        null, translateMessage('From the newest ') + fmtInt(r.sampled_requests) + translateMessage(' requests with attempt details.'));
    }
  }

  function renderWhatIf(w) {
    if (w.error) {
      $('i-whatif').textContent = '-';
      $('i-whatif-text').textContent = w.error;
      return;
    }
    const diff = num(w.difference_usd);
    $('i-whatif').textContent = fmtUSD(w.what_if_usd);
    $('i-whatif-text').textContent = translateMessage('vs ') + fmtUSD(w.current_usd) + translateMessage(' now (') + (diff <= 0 ? '' : '+') +
      fmtUSD(Math.abs(diff)).replace('$', diff < 0 ? '-$' : '$') + translateMessage(') for ') + fmtInt(w.requests) +
      translateMessage(' request(s). Same token counts assumed: tokenizers differ, quality may differ, and switching models mid-conversation rebuilds the cache.') +
      (num(w.skipped_no_price) > 0 ? ' ' + fmtInt(w.skipped_no_price) + translateMessage(' attempt(s) without a price skipped.') : '');
    $('i-whatif').title = str(w.price_source);
  }

  function renderSessions(sessions) {
    const rows = $('session-rows');
    const fragment = document.createDocumentFragment();
    for (const s of sessions) {
      const id = str(s.id);
      const tr = el('tr');
      tr.tabIndex = 0;
      tr.dataset.id = id;
      if (sessionFilter && sessionFilter.id === id) tr.className = 'selected';
      const when = el('td', fmtTime(s.last_at), 'time');
      when.append(el('span', (s.active ? translateMessage('active · ') : '') + translateMessage('started ') + fmtTime(s.started_at), 'session-tag'));
      tr.append(when);
      const model = el('td', str(s.model) || translateMessage('(none)'), 'model');
      model.title = str(s.serving_model) && str(s.serving_model) !== str(s.model) ? translateMessage('Served by ') + str(s.serving_model) : str(s.model);
      tr.append(model);
      const account = el('td', str(s.account) || '-', 'account');
      account.title = str(s.account);
      tr.append(account);
      const notes = [];
      if (num(s.errors) > 0) notes.push(fmtInt(s.errors) + translateMessage(' failed'));
      if (num(s.canceled) > 0) notes.push(fmtInt(s.canceled) + translateMessage(' canceled'));
      const requests = el('td', fmtInt(s.requests) + (notes.length ? ' (' + notes.join(', ') + ')' : ''), 'num');
      if (num(s.errors) > 0) requests.classList.add('failed-count');
      tr.append(requests);
      const ctx = el('td', undefined, 'ctx-cell');
      const gauge = ctxGauge(s.context_tokens, s.context_window, s.context_used);
      if (num(s.requests_until_full) >= 0 && num(s.context_window) > 0) {
        gauge.title += translateMessage('. At the current growth, about ') + fmtInt(s.requests_until_full) + translateMessage(' more request(s) fit.');
      }
      ctx.append(gauge);
      tr.append(ctx);
      const growth = el('td', num(s.growth_per_request) > 0 ? '+' + fmtCompact(s.growth_per_request) + translateMessage('/req') : '-', 'num opt-2');
      growth.title = translateMessage('Prompt grew from ') + fmtInt(s.first_context_tokens) + translateMessage(' to ') + fmtInt(s.context_tokens) + translateMessage(' tokens (peak ') + fmtInt(s.peak_context_tokens) + ')';
      tr.append(growth);
      const cost = el('td', costText(s), 'num');
      cost.title = costTitle(s);
      tr.append(cost);
      tr.append(el('td', str(s.client_key) || '-', 'key opt-1'));
      const session = { id, model: str(s.model), startedAt: s.started_at };
      tr.addEventListener('click', () => toggleSession(session));
      tr.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          toggleSession(session);
        }
      });
      fragment.append(tr);
    }
    rows.replaceChildren(fragment);
    const empty = sessions.length === 0;
    $('sessions-empty').hidden = !empty;
    $('sessions-wrap').hidden = empty;
  }

  function toggleSession(session) {
    sessionFilter = sessionFilter?.id === session.id ? null : session;
    renderSessionChip();
    refresh(false);
  }

  function renderSessionChip() {
    $('session-chip').hidden = !sessionFilter;
    $('session-chip-text').textContent = sessionFilter
      ? translateMessage('Session: ') + sessionFilter.model + (sessionFilter.account !== undefined
        ? ' · ' + sessionFilter.account
        : translateMessage(' · started ') + fmtTime(sessionFilter.startedAt))
      : '';
    for (const row of $('session-rows').children) row.classList.toggle('selected', !!sessionFilter && row.dataset.id === sessionFilter.id);
  }

  function renderTraces(traces) {
    const rows = $('rows');
    const fragment = document.createDocumentFragment();
    for (const t of traces) {
      const id = str(t.trace_id);
      const tr = el('tr');
      tr.tabIndex = 0;
      tr.dataset.id = id;
      if (id === selectedId) tr.className = 'selected';
      const time = el('td', fmtTime(t.started_at), 'time');
      if (num(t.session_requests) > 1) time.append(el('span', translateMessage('session request ') + fmtInt(t.session_seq) + translateMessage(' of ') + fmtInt(t.session_requests), 'session-tag'));
      tr.append(time);
      const model = el('td', str(t.requested_model) || translateMessage('(none)'), 'model');
      model.title = str(t.requested_model);
      tr.append(model);
      const provider = el('td', str(t.provider) || '-', 'provider opt-2');
      provider.title = str(t.provider);
      tr.append(provider);
      const account = el('td', str(t.account) || '-', 'account');
      account.title = str(t.account);
      tr.append(account);
      const status = el('td');
      status.append(statusBadge(t));
      tr.append(status);
      tr.append(el('td', fmtDuration(t.duration_ms), 'num'));
      const attempts = el('td', attemptsText(t), 'num');
      if (num(t.failed_attempts) > 0) attempts.classList.add('failed-count');
      tr.append(attempts);
      const tokens = el('td', fmtCompact(t.total_input_tokens) + ' / ' + fmtCompact(t.output_tokens), 'num');
      tokens.title = inputBreakdown(t) + '; ' + fmtInt(t.output_tokens) + translateMessage(' output tokens');
      tr.append(tokens);
      const cache = el('td', cacheText(t), 'cache opt-2');
      if (num(t.total_input_tokens) > 0) cache.title = translateMessage('Cache hit rate ') + fmtPercent(t.cache_hit_rate);
      tr.append(cache);
      const ctx = el('td', undefined, 'ctx-cell opt-2');
      ctx.append(ctxGauge(t.context_tokens, t.context_window, t.context_used));
      tr.append(ctx);
      const cost = el('td', costText(t), 'num');
      cost.title = costTitle(t);
      tr.append(cost);
      tr.append(el('td', str(t.client_key) || '-', 'key opt-1'));
      tr.addEventListener('click', () => select(id));
      tr.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          select(id);
        }
      });
      fragment.append(tr);
    }
    rows.replaceChildren(fragment);
    const empty = traces.length === 0;
    $('empty').hidden = !empty;
    $('table-wrap').hidden = empty;
    if (empty) {
      const filtered = $('f-status').value !== 'all' || $('f-model').value.trim() || $('f-provider').value || $('f-slow').checked || !!sessionFilter;
      $('empty').firstElementChild.textContent = filtered ? translateMessage('No traces match these filters') : translateMessage('No traces yet');
    }
  }

  function select(id) {
    selectedId = id;
    for (const row of $('rows').children) row.classList.toggle('selected', row.dataset.id === id);
    loadDetail(id, true).catch(handleError);
  }

  async function loadDetail(id, scroll) {
    let body;
    try {
      body = await api('/trace?id=' + encodeURIComponent(id));
    } catch (err) {
      if (err.status === 404) {
        // Evicted from the ring buffer since the list was loaded.
        if (selectedId === id) closeDetail();
        return;
      }
      throw err;
    }
    if (selectedId !== id || !body.trace) return;
    renderDetail(body.trace);
    if (scroll) $('detail').scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  function closeDetail() {
    selectedId = '';
    $('detail').hidden = true;
    for (const row of $('rows').children) row.classList.remove('selected');
  }

  function metaItem(list, label, value, mono) {
    const wrap = el('div');
    wrap.append(el('dt', label));
    const dd = el('dd', value === '' ? '-' : value);
    if (mono) dd.className = 'mono';
    wrap.append(dd);
    list.append(wrap);
  }

  function renderDetail(t) {
    $('detail').hidden = false;
    $('d-title').textContent = str(t.requested_model) || translateMessage('(no model)');
    const badges = $('d-badges');
    badges.replaceChildren(statusBadge(t));
    if (t.stream) badges.append(el('span', translateMessage('stream'), 'badge'));
    if (num(t.attempt_count) > 1) badges.append(el('span', translateMessage('retried'), 'badge warn'));

    const meta = $('d-meta');
    meta.replaceChildren();
    metaItem(meta, translateMessage('Trace ID'), str(t.trace_id), true);
    metaItem(meta, translateMessage('CPA request'), str(t.cpa_trace_id), true);
    metaItem(meta, translateMessage('Started'), fmtTime(t.started_at));
    metaItem(meta, translateMessage('Duration'), fmtDuration(t.duration_ms));
    metaItem(meta, translateMessage('Client format'), str(t.source_format));
    metaItem(meta, translateMessage('Attempts'), attemptsText(t));
    metaItem(meta, translateMessage('Provider'), str(t.provider));
    metaItem(meta, translateMessage('Account'), str(t.account));
    metaItem(meta, translateMessage('Tokens in / out'), fmtInt(t.total_input_tokens) + ' / ' + fmtInt(t.output_tokens));
    metaItem(meta, translateMessage('Input breakdown'), fmtInt(uncachedOf(t)) + translateMessage(' uncached · ') + fmtInt(t.cache_read_tokens) +
      translateMessage(' cache read · ') + fmtInt(t.cache_creation_tokens) + translateMessage(' cache write'));
    metaItem(meta, translateMessage('Cache hit rate'), num(t.total_input_tokens) > 0 ? fmtPercent(t.cache_hit_rate) : '-');
    metaItem(meta, translateMessage('Est. API cost'), (t.cost_unknown ? translateMessage('unknown (canceled before usage was reported)')
      : (hasRange(t) ? fmtUSD(t.cost_min_usd) + ' – ' + fmtUSD(t.cost_usd) : costText(t)) +
        (num(t.cost_without_cache_usd) > num(t.cost_usd) ? translateMessage(' (without cache ') + fmtUSD(t.cost_without_cache_usd) + ')' : '')));
    metaItem(meta, translateMessage('Price source'), str(t.price_source) ? str(t.price_source) + translateMessage(', fixed when recorded') : '');
    metaItem(meta, translateMessage('Context used'), num(t.context_window) > 0 && num(t.context_tokens) > 0
      ? fmtInt(t.context_tokens) + translateMessage(' of ') + fmtInt(t.context_window) + ' (' + fmtPercent(t.context_used) + ')'
      : (num(t.context_tokens) > 0 ? fmtInt(t.context_tokens) + translateMessage(' (window unknown)') : ''));
    metaItem(meta, translateMessage('Session'), num(t.session_requests) > 0 ? translateMessage('request ') + fmtInt(t.session_seq) + translateMessage(' of ') + fmtInt(t.session_requests) : '');
    metaItem(meta, translateMessage('Client key'), str(t.client_key), true);

    const error = $('d-error');
    error.textContent = str(t.error);
    error.hidden = !str(t.error);

    const spans = Array.isArray(t.spans) ? t.spans : [];
    const more = $('d-more');
    if (t.has_detail === false) {
      $('d-waterfall').replaceChildren(el('p', translateMessage('Attempt details are kept only for the newest requests. This older request keeps its totals, cost and session.'), 'muted'));
      $('d-attempts').replaceChildren();
      more.hidden = true;
      return;
    }
    renderWaterfall(t, spans);
    const attempts = spans.filter((s) => str(s.kind) === 'client');
    const hidden = num(t.attempt_count) - attempts.length;
    more.hidden = hidden <= 0;
    more.textContent = hidden > 0 ? hidden + translateMessage(' more attempt(s) are counted but not kept in memory.') : '';
    renderAttempts(attempts);
  }

  function renderWaterfall(t, spans) {
    const box = $('d-waterfall');
    box.replaceChildren();
    let total = num(t.duration_ms);
    for (const s of spans) total = Math.max(total, num(s.start_offset_ms) + num(s.duration_ms));
    if (total <= 0) total = 1;
    const pct = (ms) => Math.min(100, Math.max(0, (num(ms) / total) * 100));
    let ordinal = 0;
    for (const s of spans) {
      const isRoot = !str(s.parent_span_id);
      const failed = str(s.status) === 'error';
      const row = el('div', undefined, 'wf-row');
      const attrs = s.attributes && typeof s.attributes === 'object' ? s.attributes : {};
      let label;
      if (isRoot && str(s.kind) === 'server') {
        label = translateMessage('Request');
      } else {
        ordinal += 1;
        label = '#' + str(attrs['cpa.attempt'] || ordinal) + ' ' + (str(s.provider) || str(s.name));
      }
      const labelNode = el('div', label, 'wf-label');
      labelNode.title = str(s.name) + (str(s.account) ? ' - ' + str(s.account) : '');
      const track = el('div', undefined, 'wf-track');
      const bar = el('div', undefined, 'wf-bar' + (isRoot && str(s.kind) === 'server' ? ' root' : '') + (failed ? ' failed' : ''));
      // CSSOM property writes are allowed by the CSP (no inline style attributes).
      bar.style.left = pct(s.start_offset_ms) + '%';
      bar.style.width = Math.max(pct(s.duration_ms), 0.4) + '%';
      bar.title = fmtDuration(s.duration_ms) + (failed ? translateMessage(' - failed') : '');
      track.append(bar);
      for (const e of Array.isArray(s.events) ? s.events : []) {
        const mark = el('div', undefined, 'wf-mark');
        mark.style.left = pct(e.offset_ms) + '%';
        mark.title = str(e.name) + translateMessage(' at ') + fmtDuration(num(e.offset_ms) - num(s.start_offset_ms)) + translateMessage(' after attempt start');
        track.append(mark);
      }
      row.append(labelNode, track, el('div', fmtDuration(s.duration_ms), 'wf-time'));
      box.append(row);
    }
  }

  function kv(list, label, value) {
    if (value === '' || value === undefined || value === null) return;
    list.append(el('dt', label), el('dd', value));
  }

  function renderAttempts(attempts) {
    const box = $('d-attempts');
    box.replaceChildren();
    if (attempts.length === 0) {
      box.append(el('p', translateMessage('No upstream attempt was recorded (for example, the request was rejected before reaching a provider).'), 'muted'));
      return;
    }
    attempts.forEach((s, index) => {
      const a = s.attributes && typeof s.attributes === 'object' ? s.attributes : {};
      const failed = str(s.status) === 'error';
      const card = el('div', undefined, 'attempt' + (failed ? ' failed' : ''));
      const head = el('h4', translateMessage('Attempt ') + str(a['cpa.attempt'] || index + 1));
      head.append(el('span', failed ? translateMessage('failed') : 'ok', 'badge ' + (failed ? 'bad' : 'ok')));
      card.append(head);
      const list = el('dl', undefined, 'kv');
      const tok = s.tokens && typeof s.tokens === 'object' ? s.tokens : {};
      kv(list, translateMessage('Provider'), str(s.provider));
      kv(list, translateMessage('Account'), str(s.account));
      kv(list, translateMessage('Model'), str(a['gen_ai.request.model']));
      kv(list, translateMessage('Response model'), str(a['gen_ai.response.model']));
      kv(list, translateMessage('Status code'), a['http.response.status_code'] !== undefined ? str(a['http.response.status_code']) : '');
      kv(list, translateMessage('Started'), '+' + fmtDuration(s.start_offset_ms));
      kv(list, translateMessage('Latency'), fmtDuration(s.duration_ms));
      kv(list, 'TTFT', a['cpa.ttft_ms'] !== undefined ? fmtDuration(a['cpa.ttft_ms']) : '');
      kv(list, translateMessage('Input (total)'), fmtInt(tok.total_input_tokens));
      kv(list, translateMessage('Uncached input'), fmtInt(tok.uncached_input_tokens));
      kv(list, translateMessage('Cache read'), fmtInt(tok.cache_read_tokens));
      kv(list, translateMessage('Cache write'), fmtInt(tok.cache_creation_tokens));
      kv(list, translateMessage('Cache hit rate'), num(tok.total_input_tokens) > 0 ? fmtPercent(tok.cache_hit_rate) : '');
      kv(list, translateMessage('Output'), fmtInt(tok.output_tokens));
      kv(list, translateMessage('Reasoning'), num(tok.reasoning_tokens) > 0 ? fmtInt(tok.reasoning_tokens) : '');
      kv(list, translateMessage('Est. cost'), s.cost_usd !== undefined && s.cost_usd !== null ? fmtUSD(s.cost_usd) + translateMessage(' (fixed when recorded)') : '');
      kv(list, translateMessage('Auth type'), str(a['cpa.auth_type']));
      kv(list, translateMessage('Auth index'), str(a['cpa.auth_index']));
      kv(list, translateMessage('Error'), str(a['cpa.error']));
      card.append(list);

      const details = el('details');
      details.append(el('summary', translateMessage('All attributes')));
      const table = el('table', undefined, 'attr-table');
      const tbody = el('tbody');
      for (const key of Object.keys(a).sort()) {
        const row = el('tr');
        row.append(el('td', key), el('td', str(a[key])));
        tbody.append(row);
      }
      table.append(tbody);
      details.append(table);
      card.append(details);
      box.append(card);
    });
  }

  async function copyTraceID() {
    const id = selectedId;
    if (!id) return;
    const button = $('copy-id');
    let ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(id);
        ok = true;
      }
    } catch (err) {
      ok = false;
    }
    if (!ok) {
      // Fallback for iframes without clipboard permission.
      const input = el('input');
      input.value = id;
      input.readOnly = true;
      input.className = 'mono';
      document.body.append(input);
      input.select();
      try {
        ok = document.execCommand('copy');
      } catch (err) {
        ok = false;
      }
      input.remove();
    }
    button.textContent = ok ? translateMessage('Copied') : translateMessage('Copy failed');
    setTimeout(() => { button.textContent = translateMessage('Copy trace ID'); }, 1500);
  }

  // ---------- wiring ----------
  function init() {
    initLanguage();
    $('connect-btn').addEventListener('click', connect);
    $('key').addEventListener('keydown', (event) => {
      if (event.key === 'Enter') connect();
    });
    $('disconnect').addEventListener('click', () => disconnect(''));
    $('refresh').addEventListener('click', () => refresh(false));
    $('f-status').addEventListener('change', () => refresh(false));
    $('f-slow').addEventListener('change', () => refresh(false));
    $('f-provider').addEventListener('change', () => refresh(false));
    $('f-range').addEventListener('change', () => {
      const rangeStore = storage();
      try {
        if (rangeStore) rangeStore.setItem(RANGE_KEY, currentRange());
      } catch (err) {
        // Storage may be blocked; the choice then lasts for this page only.
      }
      refresh(false);
    });
    $('i-whatif-model').addEventListener('change', () => {
      const range = encodeURIComponent(currentRange());
      api('/whatif?range=' + range + '&model=' + encodeURIComponent($('i-whatif-model').value))
        .then(renderWhatIf, (err) => renderWhatIf({ error: err.message }));
    });
    $('session-clear').addEventListener('click', () => {
      sessionFilter = null;
      renderSessionChip();
      refresh(false);
    });
    $('f-model').addEventListener('input', () => {
      clearTimeout(modelDebounce);
      modelDebounce = setTimeout(() => refresh(false), 300);
    });
    $('f-auto').addEventListener('change', schedule);
    $('close-detail').addEventListener('click', closeDetail);
    $('copy-id').addEventListener('click', copyTraceID);
    document.addEventListener('visibilitychange', () => {
      if (!managementKey) return;
      if (document.visibilityState === 'visible' && $('f-auto').checked) refresh(false);
      schedule();
    });

    try {
      const rangeStore = storage();
      const savedRange = rangeStore ? rangeStore.getItem(RANGE_KEY) : null;
      if (savedRange && Object.prototype.hasOwnProperty.call(RANGE_LABELS, savedRange)) $('f-range').value = savedRange;
    } catch (err) {
      // Storage blocked: keep the default range.
    }
    const store = storage();
    const remembered = store ? store.getItem(SESSION_KEY) : null;
    if (remembered) {
      $('remember').checked = true;
      $('key').value = remembered;
      connect();
    } else {
      $('key').focus();
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
