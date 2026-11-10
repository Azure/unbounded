//! Writer-side staging of encrypted pages until their store targets pull them.
//!
//! A writer encrypts each page once and keeps the ciphertext here until every
//! store target has acknowledged a durable copy or the write ends. Store
//! targets pull a staged page with an ordinary directed copy-only `page`
//! request, which [`crate::read::fill::Fill::copy_only_joining`] answers from
//! this table before any cache lookup. Staging never writes to disk.
//!
//! The table is node-wide: the worker that owns a page (and so serves its
//! copy-only requests) is usually not the worker reading the client body.
//! Staged ciphertext stays charged to the writer worker's `Ciphertext`
//! admission class, which bounds staging per worker; the writer also keeps
//! at most `RACER_WRITE_WINDOW_PAGES` pages staged per request.

use crate::memory::CiphertextCopy;
use crate::model::PageId;
use std::collections::HashMap;
use std::sync::Mutex;

/// Staged ciphertext by page, shared by every worker of a node.
#[derive(Default)]
pub struct WriteStaging {
    pages: Mutex<HashMap<PageId, CiphertextCopy>>,
}

impl WriteStaging {
    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<PageId, CiphertextCopy>> {
        self.pages.lock().unwrap_or_else(|poison| poison.into_inner())
    }

    /// Stage `copy` under its envelope's page. A page already staged (the
    /// same version is never written twice) is replaced.
    pub fn insert(&self, copy: CiphertextCopy) {
        let page = copy.ciphertext.envelope().page.clone();
        self.lock().insert(page, copy);
    }

    /// The staged copy of `page`, if any.
    pub fn get(&self, page: &PageId) -> Option<CiphertextCopy> {
        self.lock().get(page).cloned()
    }

    /// Stop staging `page`, returning whether it was staged.
    pub fn remove(&self, page: &PageId) -> bool {
        self.lock().remove(page).is_some()
    }

    /// Number of staged pages.
    pub fn len(&self) -> usize {
        self.lock().len()
    }

    /// Whether nothing is staged.
    pub fn is_empty(&self) -> bool {
        self.lock().is_empty()
    }
}

/// Removes a page from staging when dropped, so every exit path of a write
/// (success, failure, cancellation) unstages its pages.
pub struct Staged<'a> {
    staging: &'a WriteStaging,
    page: PageId,
}

impl<'a> Staged<'a> {
    /// Stage `copy` until the guard drops.
    pub fn new(staging: &'a WriteStaging, copy: CiphertextCopy) -> Self {
        let page = copy.ciphertext.envelope().page.clone();
        staging.insert(copy);
        Self { staging, page }
    }
}

impl Drop for Staged<'_> {
    fn drop(&mut self) {
        self.staging.remove(&self.page);
    }
}
