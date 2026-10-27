// Deleting a project needs the upload token: /ui has no authentication of its
// own, so the page sends the token the user enters to the API, the same as an
// upload. The token is kept for the browser session only.
(() => {
    const root = document.getElementById('project-delete');
    if (!root) {
        return;
    }
    const start = document.getElementById('project-delete-start');
    const form = document.getElementById('project-delete-form');
    const input = document.getElementById('project-delete-token');
    const cancel = document.getElementById('project-delete-cancel');
    const errorBox = document.getElementById('project-delete-error');
    const submit = form.querySelector('button[type="submit"]');
    const storageKey = 'projects-token';

    const show = (open) => {
        start.classList.toggle('hidden', open);
        form.classList.toggle('hidden', !open);
        form.classList.toggle('flex', open);
        errorBox.classList.add('hidden');
    };

    start.addEventListener('click', () => {
        try {
            input.value = sessionStorage.getItem(storageKey) || '';
        } catch (e) {
            input.value = '';
        }
        show(true);
        input.focus();
    });
    cancel.addEventListener('click', () => show(false));

    form.addEventListener('submit', async (event) => {
        event.preventDefault();
        const url = '/api/projects/' + encodeURIComponent(root.dataset.project);
        submit.disabled = true;
        errorBox.classList.add('hidden');
        try {
            const resp = await fetch(url, {
                method: 'DELETE',
                headers: { Authorization: 'Bearer ' + input.value },
            });
            if (resp.status === 401) {
                throw new Error('the token was rejected');
            }
            if (!resp.ok) {
                const body = await resp.json().catch(() => ({}));
                throw new Error(body.message || body.error || `HTTP ${resp.status}`);
            }
            try {
                sessionStorage.setItem(storageKey, input.value);
            } catch (e) {
                // Without storage the token is just asked for again.
            }
            window.location.href = '/ui/projects';
        } catch (err) {
            errorBox.textContent = 'Delete failed: ' + err.message;
            errorBox.classList.remove('hidden');
            submit.disabled = false;
        }
    });
})();
