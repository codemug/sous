// internal/ui/static/dragdrop.js
//
// Vanilla drag-and-drop: dragging a recipe card onto a node card's capacity
// indicator posts a deploy request. No framework - this project's UI has
// been plain html/template + CSS with zero client-side JS until this file;
// keep it that way rather than pulling in a drag-drop library for one
// interaction.
(function () {
  document.querySelectorAll('[data-recipe-id]').forEach(function (card) {
    card.addEventListener('dragstart', function (e) {
      e.dataTransfer.setData('text/recipe-id', card.dataset.recipeId);
      e.dataTransfer.effectAllowed = 'move';
    });
  });

  document.querySelectorAll('[data-node-id]').forEach(function (target) {
    target.addEventListener('dragover', function (e) {
      // Required for the element to become a valid drop target at all -
      // without it the browser rejects the drop before "drop" ever fires.
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      target.classList.add('drop-hover');
    });
    target.addEventListener('dragleave', function () {
      target.classList.remove('drop-hover');
    });
    target.addEventListener('drop', function (e) {
      e.preventDefault();
      target.classList.remove('drop-hover');
      var recipeId = e.dataTransfer.getData('text/recipe-id');
      if (!recipeId) return;
      var nodeId = target.dataset.nodeId;
      if (!nodeId) return;
      fetch('/api/deploy/' + encodeURIComponent(recipeId) + '/' + encodeURIComponent(nodeId), {
        method: 'POST',
      }).then(function (res) {
        if (!res.ok) {
          // No capacity, no connection, unknown node/recipe - whatever the
          // refusal is, the server's own error text is the accurate one;
          // this alert is the "some feedback" the drop needs rather than a
          // silently swallowed failure. A real toast/inline banner would be
          // nicer, but this page has never carried any client-side UI state
          // to hang one off, and alert() costs nothing new.
          return res.text().then(function (msg) {
            alert('Deploy failed: ' + msg);
          });
        }
        location.reload();
      }).catch(function (err) {
        alert('Deploy failed: ' + err);
      });
    });
  });
})();
