/* graphd — lodash shim.
 *
 * The vendored cytoscape-edgehandles UMD build declares two lodash functions as
 * *external* modules: `memoize` and `throttle`. Its browser branch resolves them
 * from `root._.memoize` / `root._.throttle`, so it needs a global `_` carrying
 * exactly those two functions.
 *
 * Rather than vendor the whole of lodash (~70KB) for two helpers, this file
 * provides them. It is 40 lines instead of a dependency, which is the same
 * trade the rest of the project makes. This is the one file beyond the four in
 * SPEC §3.4; it is committed to the repo and needs no build step, like the rest.
 *
 * It must load before cytoscape-edgehandles.js and after cytoscape.min.js.
 */
(function (global) {
  'use strict';

  // memoize(fn, resolver?) — caches results. edgehandles uses it to memoize
  // per-target edge-parameter lookups; the default resolver is the first arg.
  function memoize(fn, resolver) {
    var cache = {};
    var memoized = function () {
      var key = String(resolver ? resolver.apply(this, arguments) : arguments[0]);
      if (!Object.prototype.hasOwnProperty.call(cache, key)) {
        cache[key] = fn.apply(this, arguments);
      }
      return cache[key];
    };
    memoized.cache = cache;
    return memoized;
  }

  // throttle(fn, wait) — at most one call per `wait` ms, with a trailing call so
  // the final position is never dropped. edgehandles uses it for its snap
  // handler; it never calls .cancel()/.flush() on the result.
  function throttle(fn, wait) {
    var last = 0;
    var timer = null;
    var lastArgs = null;
    var lastThis = null;

    function invoke() {
      last = Date.now();
      timer = null;
      fn.apply(lastThis, lastArgs);
      lastArgs = lastThis = null;
    }

    function throttled() {
      var now = Date.now();
      var remaining = wait - (now - last);
      lastArgs = arguments;
      lastThis = this;
      if (remaining <= 0 || remaining > wait) {
        if (timer) {
          clearTimeout(timer);
          timer = null;
        }
        invoke();
      } else if (!timer) {
        timer = setTimeout(invoke, remaining);
      }
    }

    throttled.cancel = function () {
      if (timer) clearTimeout(timer);
      timer = null;
      last = 0;
      lastArgs = lastThis = null;
    };
    return throttled;
  }

  global._ = global._ || {};
  global._.memoize = global._.memoize || memoize;
  global._.throttle = global._.throttle || throttle;
})(typeof window !== 'undefined' ? window : this);
