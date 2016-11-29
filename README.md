# Overview

JSON/HTTP service which fetches resources identified by URLs. Returns page type, title, description, and favicon.

# How to Run

    $ ./links-parser

Here is an example request:

    $ curl -d '{"request": [{"url": "http://www.google.com"}]}' -H 'content-type: application/json' localhost:3000

links-parser will try to save results to Redis when available.

# How to Test

    $ make test

# Notes

- rootUrl in response is to be used as a unique identifier for the page.
- Uses go's charset to try and auto detect page encoding and convert to UTF8.
- Limited to 10 redirects in a row.
