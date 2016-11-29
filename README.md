# Overview

JSON/HTTP service which fetches resources identified by URLs. Returns page type, title, description, and favicon.

# How to Run

    $ ./oh-augmentation-links

Here is an example request:

    $ curl -d '{"request": [{"url": "http://www.google.com"}]}' -H 'content-type: application/json' localhost:3000

oh-augmentation-links will try to save results to Redis when available.

# How to Test

    $ make test

# Benchmarking

See benchmark/links_benchmark.py

Sample results comparing against previous node implementation:

**Node**
    Links attempted:  171
      Responses:  163
      Successes:  56
    Total fetch time:  110776
      min: 744 max: 4652
      avg: 1978.14
    Total parse time:  24
      min: 0 max: 2
      avg: 0.43

**Go**
    Links attempted:  171
      Responses:  175
      Successes:  162
    Total fetch time:  94617
      min: 13 max: 2987
      avg: 584.06
    Total parse time:  20613
      min: 0 max: 2682
      avg: 127.24

Go runs faster overall, although the bottleneck is still the time it takes to fetch pages. More significantly, go's success rate appears to be much better. Node version also appears to report false parsing times.

# Notes

- rootUrl in response is to be used as a unique identifier for the page.
- Uses go's charset to try and auto detect page encoding and convert to UTF8.
- Limited to 10 redirects in a row.
