'use strict';

// syncChildren makes parent hold exactly the wanted nodes, in order, while
// leaving every node that is already in place untouched. A node that stays
// mounted keeps keyboard focus, scroll position and loaded images, which a
// wholesale replacement of the children throws away.
function syncChildren(parent, wanted) {
  var i;
  var current = Array.prototype.slice.call(parent.childNodes);
  // Leaving nodes go first, so a kept node never has to move past one.
  for (i = 0; i < current.length; i++) {
    if (wanted.indexOf(current[i]) === -1) parent.removeChild(current[i]);
  }
  for (i = 0; i < wanted.length; i++) {
    if (parent.childNodes[i] !== wanted[i]) {
      parent.insertBefore(wanted[i], parent.childNodes[i] || null);
    }
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { syncChildren: syncChildren };
}
