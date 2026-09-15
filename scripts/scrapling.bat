@echo off
rem Windows shim exposing scripts\scrape.mjs as the "scrapling" CLI contract
rem (see scripts/scrape.mjs). Point SCRAPLING_BIN at this file, or add
rem scripts\ to PATH. Requires Node 18+; fetch modes need
rem npm install && npx playwright install chromium.
node "%~dp0scrape.mjs" %*
