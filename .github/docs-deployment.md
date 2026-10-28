# Docs deployment

## Production

`docs.yaml` deploys the docs from the latest published stable release tag, not
from `main`. Drafts, prereleases, and older releases do not replace production.
Publishing a release by hand triggers the workflow. The release upgrade workflow
calls it after publishing a release because events created with `GITHUB_TOKEN`
do not start new release workflows.

Keep this repository's Pages source set to **GitHub Actions**. In the
`github-pages` environment, allow the `main` branch and tags matching `v*`.
Automated releases run the workflow from `main`; release events run from a tag.
Keep any existing required reviewers.

To retry a docs deployment after fixing a failure, run **unbounded docs** from
`main` and enter the latest published stable release's tag. For example:

```sh
gh workflow run docs.yaml --ref main -f tag=v0.10.0
```

This still builds from the release tag. The workflow checks the release before
building, so a retry cannot deploy unpublished changes from `main`.

## Staging

GitHub Pages supports [one site per repository](https://docs.github.com/en/pages/getting-started-with-github-pages/what-is-github-pages).
The `docs-staging.yaml` workflow can publish `main` to a separate repository.
It is disabled until `DOCS_STAGING_REPOSITORY` is set and never uses this
repository's production Pages deployment.

To enable it:

1. Create a repository just for the staging site, such as
   `Azure/unbounded-docs-staging`. Its `gh-pages` branch will contain only generated
   site files. Each deployment replaces those files.
2. Create an SSH deploy key. Add the public key to the staging repository with
   **Allow write access** enabled. Do not reuse a key that can write to production.
3. Create a `docs-staging` environment in this repository, limited to the `main`
   branch. Store the private key there as `DOCS_STAGING_DEPLOY_KEY`.
4. Set these **repository variables** in this repository:

   | Variable | Example |
   | --- | --- |
   | `DOCS_STAGING_REPOSITORY` | `Azure/unbounded-docs-staging` |
   | `DOCS_STAGING_BASE_URL` | `https://azure.github.io/unbounded-docs-staging/` |

   Use the staging site's actual HTTPS URL, including its path and trailing slash.
5. Run **unbounded docs staging** from `main` to create the `gh-pages` branch.
   In the staging repository's Pages settings, select **Deploy from a branch**,
   then `gh-pages` and `/ (root)`. Run the workflow again if needed to start the
   first Pages build.

Later docs changes on `main` update staging automatically. You can also run the
workflow from `main` after changing its settings. The deploy action adds
`.nojekyll`, so Pages serves the Hugo output without running Jekyll.

Staging page titles include `(staging)` and pages ask search engines not to index
them. This is not access control; assume the site is public. Staging uses a
separate deploy key and concurrency group, so it cannot replace or cancel a
production deployment. Remove `DOCS_STAGING_REPOSITORY` to disable new staging
runs; this does not remove a site that is already published.
