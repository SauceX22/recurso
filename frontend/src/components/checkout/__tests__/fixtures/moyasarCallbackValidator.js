// The callback validator from the pinned moyasar-payment-form 2.3.0 bundle
// (dist/moyasar.umd.min.js, minified names `Dr` and `h`), copied verbatim
// apart from the export and formatting. Moyasar.init runs it before anything
// else and throws when a callback isn't an async function. Re-copy it when
// MOYASAR_FORM_VERSION changes.
const h = (A) => {
  throw new Error(A);
};

export const validateMoyasarCallbacks = (A) => {
  function e(t) {
    if (typeof t != "function") return !1;
    const n = (t == null ? void 0 : t.constructor.prototype.constructor.name) === "AsyncFunction",
      r = t.toString().startsWith("async");
    return n || r;
  }
  return (
    A.on_initiating &&
      !e(A.on_initiating) &&
      h("On initiating must be a promise based callback, check documentation for implementation details"),
    A.on_completed &&
      !e(A.on_completed) &&
      h("On completed must be a promise based callback, check documentation for implementation details"),
    A.on_failure &&
      !e(A.on_failure) &&
      h("On failure must be a promise based callback, check documentation for implementation details"),
    A.on_redirect &&
      !e(A.on_redirect) &&
      h("On redirect must be a promise based callback, check documentation for implementation details"),
    !0
  );
};
