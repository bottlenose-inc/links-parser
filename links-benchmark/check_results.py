#!/usr/bin/env python

import sys
import json

FIELDS_CHECK = [
    'rootUrl',
    'providerName',
    'title',
    'description',
]

if __name__ == '__main__':

    if len(sys.argv) < 3:
        print 'Usage: ./check_results.py [ext1] [ext2]'
        sys.exit(0)

    ext1 = sys.argv[1]
    ext2 = sys.argv[2]

    keys1 = [key.strip() for key in open("results/keys_%s" % ext1, 'r')]
    keys2 = [key.strip() for key in open("results/keys_%s" % ext2, 'r')]

    missingKeys = []
    uncheckedKeys = []

    matchedKeys = []
    mismatch = {}

    for key in keys2:
        if key not in keys1:
            uncheckedKeys.append(key)

    for key in keys1:
        if key not in keys2:
            missingKeys.append(key)
            continue

        file1 = open('results/%s_%s' % (key, ext1), 'r')
        file2 = open('results/%s_%s' % (key, ext2), 'r')

        result1 = file1.readline()
        result2 = file2.readline()
        result1 = json.loads(result1)
        result2 = json.loads(result2)

        for field in FIELDS_CHECK:
            val1 = result1.get(field, None)
            val2 = result2.get(field, None)
            if val1 != val2:
                if key not in mismatch:
                    mismatch[key] = {}
                mismatch[key][field] = [val1, val2]
            
        if key not in mismatch: 
            matchedKeys.append(key)

    print "%s keys: %d" % (ext1, len(keys1))
    print "%s keys: %d" % (ext2, len(keys2))
    print "Matched: %d" % len(matchedKeys)
    print "Missing (in %s, not %s): %s" % (ext1, ext2, str(missingKeys))
    print "Unchecked (in %s, not %s): %s" % (ext2, ext1, str(uncheckedKeys))
    print "Mismatches saved in mismatches.txt"

    misFile = open("mismatches.txt", "w")
    misFile.write( json.dumps(mismatch) )
    misFile.close()
    
