import RFB from './vendor/core/rfb.js';
const status = document.querySelector('#status');
const control = document.querySelector('#control');
const keyboard = document.querySelector('#keyboard');
const mobile = document.querySelector('#mobile');
const url = new URL('ws', location.href);
url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
const rfb = new RFB(document.querySelector('#screen'), url.href);
rfb.viewOnly = true;
rfb.scaleViewport = true;
rfb.resizeSession = false;
let human = false;
rfb.addEventListener('connect', () => { status.textContent = 'Watching · read only'; control.disabled = false; });
rfb.addEventListener('disconnect', () => { status.textContent = 'Disconnected. Reload to reconnect.'; control.disabled = keyboard.disabled = true; document.body.classList.remove('human'); });
control.addEventListener('click', async () => {
  control.disabled = true;
  const next = !human;
  try {
    // Freeze local input first when releasing; enable only after the server
    // has acknowledged its VNC input policy.
    if (!next) rfb.viewOnly = true;
    const response = await fetch('control', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({human_control: next})});
    if (!response.ok) throw new Error(await response.text());
    human = next;
    rfb.viewOnly = !human;
    keyboard.disabled = !human;
    control.textContent = human ? 'Release control' : 'Take over';
    status.textContent = human ? 'You are in control' : 'Watching · read only';
    document.body.classList.toggle('human', human);
    if (human) rfb.focus();
  } catch (error) { status.textContent = `Control failed: ${error.message}`; }
  finally { control.disabled = false; }
});
keyboard.addEventListener('click', () => mobile.focus());
mobile.addEventListener('input', () => {
  if (human) for (const char of mobile.value) {
    const code = char.codePointAt(0);
    rfb.sendKey(code < 256 ? code : 0x01000000 | code);
  }
  mobile.value = '';
});
mobile.addEventListener('keydown', event => {
  if (human && ['Enter', 'Backspace'].includes(event.key)) {
    rfb.sendKey(event.key === 'Enter' ? 0xff0d : 0xff08);
    event.preventDefault();
  }
});
