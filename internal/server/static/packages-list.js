document.getElementById('sort-by').addEventListener('change', function(e) {
    const params = new URLSearchParams(window.location.search);
    params.set('sort', e.target.value);
    params.delete('page');
    window.location.href = '/ui/packages?' + params.toString();
});
