#!/usr/bin/env python

import sys
import json

try:
    namesfile = open('providers.txt', 'r')
except:
    print('providers.txt not found!')
    sys.exit(0)

data = {}

for line in namesfile:
    link, name = line.split(', ')
    data[link.strip()] = name.strip()

namesfile.close()

with open('providers.json', 'w') as f:
    f.write( json.dumps(data, indent=4, separators=(',', ': ')) )
