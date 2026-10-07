/* El Pulpo dashboard — the error path htmx does not have.

   htmx 1.9 swaps a response into its target only when the status is 2xx/3xx:
   a refused mutation (400 broken field rule, 409 hash changed on disk, 422
   validation violations, 403 stale CSRF marker) fires htmx:responseError and
   changes nothing on screen, so a save the server refused looked exactly like
   a button that does nothing. This file is the one script the dashboard links,
   same-origin under `default-src 'self'` — no inline script, and no hx-on
   either, which would need eval. It issues no request of its own: it reads the
   answer htmx already has in hand and writes it back as text — textContent,
   never markup — directly under the button that was clicked. */
(function () {
	'use strict';

	var MSG = 'form-msg';
	var STALE = 'elpulpo-refresh-error';
	var SUBMIT = 'button:not([type]), button[type="submit"], input[type="submit"]';

	/* --- which button caused this request -----------------------------------
	   htmx hands the event the form, not the button whose click submitted it,
	   and the message belongs under the button that was clicked. The click is
	   therefore noted on the capture phase, ahead of htmx's delegated handler.
	   Enter in a text field submits with the form's first button, which is
	   what the fallback in triggerFor reproduces. */

	var clicked = new WeakMap();

	document.addEventListener('click', function (evt) {
		var target = evt.target;
		if (!target || !target.closest) return;
		var btn = target.closest(SUBMIT);
		if (btn && btn.form) clicked.set(btn.form, btn);
	}, true);

	/* --- wording -------------------------------------------------------------
	   Each message names what it is about, because a message left on screen
	   while the operator scrolls must still read as its own. */

	var REFUSED = {
		'host/save': 'Host not saved',
		'host/delete': 'Host not deleted',
		'server/save': 'Server not saved',
		'server/delete': 'Server not deleted',
		'route/save': 'Route not saved',
		'route/delete': 'Route not deleted',
		'prices/save': 'Prices not saved',
		'settings/save': 'Settings not saved',
		'config/save': 'Configuration not saved',
		'config/import': 'Import not accepted',
		'config/import/apply': 'Import not applied',
		'catalog/apply': 'Catalogue rates not applied',
		'prune/run': 'Nothing was pruned',
		'debug/save': 'Debug settings not saved',
		'debug/clear': 'Nothing was cleared'
	};

	var ACCEPTED = {
		'host/save': 'Host saved.',
		'host/delete': 'Host deleted.',
		'server/save': 'Server saved.',
		'server/delete': 'Server deleted.',
		'route/save': 'Route saved.',
		'route/delete': 'Route deleted.',
		'prices/save': 'Prices saved.',
		'settings/save': 'Settings saved.',
		'config/save': 'Configuration saved.',
		'config/import/apply': 'Import applied: the whole document was replaced.',
		'catalog/apply': 'Catalogue rates applied.',
		'debug/save': 'Debug settings saved.',
		'debug/clear': 'Recordings cleared.'
	};

	function actionName(detail) {
		var info = detail.pathInfo || {};
		var cfg = detail.requestConfig || {};
		var path = String(info.requestPath || cfg.path || '');
		var i = path.indexOf('/action/');
		return i < 0 ? '' : path.slice(i + 8).split('?')[0];
	}

	function refused(action) {
		return REFUSED[action] || 'The server refused this';
	}

	/* --- the message node ----------------------------------------------------
	   One node per form, reused, placed as the sibling right after the trigger,
	   so it sits under the button that caused the answer — including inside a
	   table cell, where a row's own buttons live. */

	function requestForm(elt) {
		if (!elt || !elt.tagName) return null;
		if (elt.tagName === 'FORM') return elt;
		if (elt.form) return elt.form;
		return elt.closest ? elt.closest('form') : null;
	}

	function isSubmitNode(node) {
		return !!(node && node.matches && node.matches(SUBMIT));
	}

	function triggerFor(detail, form) {
		var elt = detail.elt;
		if (isSubmitNode(elt)) return elt; // hx-post on the button itself
		if (!form) return null;
		var btn = clicked.get(form);
		if (btn && form.contains(btn)) return btn;
		var btns = form.querySelectorAll(SUBMIT);
		return btns.length ? btns[0] : null;
	}

	function clearMessage(form) {
		if (!form) return;
		var nodes = form.querySelectorAll('.' + MSG);
		for (var i = 0; i < nodes.length; i++) {
			if (nodes[i].parentNode) nodes[i].parentNode.removeChild(nodes[i]);
		}
	}

	function messageNode(form, elt) {
		var host = form || (elt && elt.parentNode) || document.body;
		var found = host.querySelectorAll('.' + MSG);
		if (found.length) return found[0];
		var node = document.createElement('div');
		node.className = MSG;
		node.setAttribute('role', 'status');
		node.setAttribute('aria-live', 'polite');
		host.appendChild(node);
		return node;
	}

	function show(form, elt, trigger, kind, lines) {
		var node = messageNode(form, elt);
		node.className = MSG + ' ' + kind;
		while (node.firstChild) node.removeChild(node.firstChild);
		for (var i = 0; i < lines.length; i++) {
			var line = document.createElement('div');
			line.textContent = lines[i];
			node.appendChild(line);
		}
		// The node goes right after the button that caused the answer, in
		// whatever holds it — a table cell when it is a row's own button.
		if (trigger && trigger.parentNode && trigger.nextSibling !== node) {
			trigger.parentNode.insertBefore(node, trigger.nextSibling);
		}
	}

	/* --- reading the answer htmx already has --------------------------------- */

	function bodyOf(xhr) {
		var text = '';
		try {
			text = (xhr && xhr.responseText) || '';
		} catch (e) {
			return null; // a response the browser will not hand over
		}
		if (!text) return null;
		try {
			var v = JSON.parse(text);
			return v && typeof v === 'object' ? v : null;
		} catch (e) {
			return null;
		}
	}

	function violationLines(vs) {
		var out = [];
		for (var i = 0; i < vs.length && i < 12; i++) {
			var v = vs[i] || {};
			var line = '';
			if (v.path) line += v.path + ': ';
			line += v.msg === undefined || v.msg === null ? 'invalid value' : String(v.msg);
			if (v.line) line += ' (line ' + v.line + ')';
			out.push(line);
		}
		if (vs.length > 12) out.push('… and ' + (vs.length - 12) + ' more');
		return out;
	}

	function errorLines(action, status, body) {
		var head = refused(action);
		if (body && body.violations && body.violations.length) {
			var n = body.violations.length;
			var lines = [head + ' — ' + n + ' problem' + (n === 1 ? '' : 's') + ':'];
			return lines.concat(violationLines(body.violations));
		}
		var msg;
		if (body && body.error) msg = String(body.error);
		else if (status) msg = 'the server answered HTTP ' + status;
		else msg = 'no answer from El Pulpo — it may be restarting';
		if (status === 403) {
			// "csrf check failed" is true but useless to an operator: what
			// happened is that this page predates the current session marker.
			msg += ' — this page carries an older session marker; reload it and try again';
		}
		if (status === 401) {
			// The dashboard asks for credentials of its own accord; a 401 on a
			// mutation means that handshake went away while the page stayed.
			msg += ' — reload the page and sign in again';
		}
		return [head + ' — ' + msg];
	}

	/* The success bodies are JSON the contract fixes; the raw text is never
	   worth showing, so each shape gets the one line an operator reads. */
	function successText(action, body) {
		if (action === 'debug/clear' && body.removed !== undefined) {
			return 'Cleared ' + body.removed + ' recording' + (body.removed === 1 ? '' : 's') + '.';
		}
		if (body.removed !== undefined) {
			return 'Pruned ' + body.removed + ' usage row' + (body.removed === 1 ? '' : 's') + '.';
		}
		if (body.preview) return 'Preview ready — nothing has been applied yet.';
		if (ACCEPTED[action]) return ACCEPTED[action];
		if (body.ok) return 'Saved.';
		return null; // an unknown shape: leave whatever htmx swapped alone
	}

	function names(list) {
		return (list && list.length ? list : []).join(', ');
	}

	/* The import preview is the one success body worth more than a line: the
	   panel sits right under the buttons, so the readable preview replaces the
	   JSON the swap would otherwise leave there. */
	function previewLines(preview) {
		var lines = ['Preview of this import — nothing applied yet:'];
		var sections = [
			['hosts', preview.added_hosts, preview.changed_hosts, preview.removed_hosts],
			['routes', preview.added_routes, preview.changed_routes, preview.removed_routes]
		];
		var unchanged = true;
		for (var i = 0; i < sections.length; i++) {
			var s = sections[i];
			var parts = [];
			if (s[1] && s[1].length) parts.push('added ' + names(s[1]));
			if (s[2] && s[2].length) parts.push('changed ' + names(s[2]));
			if (s[3] && s[3].length) parts.push('removed ' + names(s[3]));
			if (parts.length) unchanged = false;
			lines.push(s[0] + ' — ' + (parts.length ? parts.join(' · ') : 'unchanged'));
		}
		if (unchanged) lines.push('Nothing would change.');
		return lines;
	}

	/* htmx swapped the JSON body into the form's hx-target; once it has been
	   turned into a message, the raw text goes. */
	function replaceTarget(detail, body) {
		var target = detail.target;
		var text = '';
		try {
			text = (detail.xhr && detail.xhr.responseText) || '';
		} catch (e) {
			return;
		}
		if (!target || target.nodeType !== 1) return;
		// htmx put the body in there verbatim; anything else that has appeared
		// in the meantime is the operator's, not ours to clear. A JSON body
		// read back as HTML loses its angle brackets, hence the looser test.
		var shown = (target.textContent || '').trim();
		if (shown !== (text || '').trim() && shown.charAt(0) !== '{') return;
		target.textContent = '';
		if (body && body.preview) {
			var lines = previewLines(body.preview);
			for (var i = 0; i < lines.length; i++) {
				var node = document.createElement('div');
				node.textContent = lines[i];
				target.appendChild(node);
			}
		}
	}

	/* --- a failed read is a different thing ----------------------------------
	   The fragments refresh themselves; a failing one leaves stale figures on
	   screen, which has to be said where it cannot be missed. */

	var failing = {};

	function refreshBanner() {
		var main = document.querySelector('main');
		if (!main) return null;
		var node = document.getElementById(STALE);
		if (!node) {
			node = document.createElement('div');
			node.id = STALE;
			node.className = 'banner error';
			node.setAttribute('role', 'alert');
			main.insertBefore(node, main.firstChild);
		}
		return node;
	}

	function readFailed(path, status) {
		failing[path] = true;
		var node = refreshBanner();
		if (!node) return;
		node.textContent = 'Refresh failed (HTTP ' + (status || '?') +
			') — the numbers left on screen are from the last good read.';
	}

	function readRecovered(path) {
		if (!failing[path]) return;
		delete failing[path];
		for (var k in failing) {
			if (Object.prototype.hasOwnProperty.call(failing, k)) return;
		}
		var node = document.getElementById(STALE);
		if (node && node.parentNode) node.parentNode.removeChild(node);
	}

	/* --- wiring --------------------------------------------------------------- */

	document.addEventListener('htmx:beforeRequest', function (evt) {
		clearMessage(requestForm(evt.detail.elt));
	});

	document.addEventListener('htmx:afterRequest', function (evt) {
		var detail = evt.detail || {};
		var cfg = detail.requestConfig || {};
		var verb = String(cfg.verb || '').toLowerCase();
		var path = (detail.pathInfo || {}).requestPath || cfg.path || '';

		if (verb !== 'post') {
			// Reads: the polls and the stats query. Only their failure matters.
			if (detail.successful) readRecovered(path);
			else if (detail.failed) readFailed(path, detail.xhr && detail.xhr.status);
			return;
		}

		var form = requestForm(detail.elt);
		var trigger = triggerFor(detail, form);
		var action = actionName(detail);

		if (detail.successful) {
			var body = bodyOf(detail.xhr);
			var text = body ? successText(action, body) : null;
			if (!text) return; // not one of our JSON shapes: htmx's swap stands
			replaceTarget(detail, body);
			show(form, detail.elt, trigger, 'ok', [text]);
			return;
		}

		if (!detail.failed && !detail.xhr) return; // cancelled before it was sent
		var status = detail.failed && detail.xhr ? detail.xhr.status : 0;
		show(form, detail.elt, trigger, 'error', errorLines(action, status, bodyOf(detail.xhr)));
	});
})();
