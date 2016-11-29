#!/usr/bin/env python

# create results folder
# first run ./links_benchark.py testlinks.txt go/node
# then run ./check_results.py node go


import sys
import datetime
import json
import gnsq
import requests
import hashlib
import signal

NSQ_ADDR = '127.0.0.1'
NSQ_PORT = 4150
LINKS_ADDR = 'http://links:3000/'

class LinksBenchmark(object):

    def __init__(self, outExt):
        self.outExt = outExt
        self.attempts = 0
        self.responses = 0
        self.successes = 0
        self.fetchTimes = [0]
        self.parseTimes = [0]

        self.startTime = datetime.datetime.now()
        self.lastTime = datetime.datetime.now()

        if self.outExt:
            self.keysFile = open("results/keys_%s" % self.outExt, "w")
        else:
            self.keysFile = None

    def __del__(self):
        if self.keysFile:
            self.keysFile.close()

    def inc_attempts(self):
        self.attempts = self.attempts + 1

    def add_result(self, key, result):
        if 'response' not in result:
            print '-----', result
            return
        self.responses = self.responses + 1
        resp = result['response']
        if 'error' in resp:
            print '*****', resp
            return
        if 'link' not in resp[0]:
            print '^^^^^', resp
            return
        link = resp[0]['link']
        if 'fetchDuration' not in link or 'parseDuration' not in link:
            print '>>>>>', resp[0]
            return

        fetchDuration = int(link.get('fetchDuration', 0))
        parseDuration = int(link.get('parseDuration', 0))
        if self.successes == 0:
            self.fetchTimes = [ fetchDuration ]
            self.parseTimes = [ parseDuration ]
        else:
            self.fetchTimes.append( fetchDuration )
            self.parseTimes.append( parseDuration )
        self.successes = self.successes + 1

        self.lastTime = datetime.datetime.now()

        if self.keysFile:
            outFile = open("results/%s_%s" % (key, self.outExt), "w")
            outFile.write( json.dumps(link) )
            outFile.close()

            self.keysFile.write( key + '\n' )

    def print_stats(self):
        print 'Links attempted: ', self.attempts
        print '  Responses: ', self.responses
        print '  Successes: ', self.successes
        print 'Total fetch time: ', sum(self.fetchTimes)
        print '  min: %d max: %d' % (min(self.fetchTimes), max(self.fetchTimes))
        print '  avg: %.2f' % self.average(self.fetchTimes)
        print 'Total parse time: ', sum(self.parseTimes)
        print '  min: %d max: %d' % (min(self.parseTimes), max(self.parseTimes))
        print '  avg: %.2f' % self.average(self.parseTimes)
        elapsed = self.lastTime - self.startTime
        mins, secs = divmod(elapsed.total_seconds(), 60)
        print 'Time from start to last success: %d mins %f secs' % (mins, secs)

    def average(self, times):
        return float(sum(times)) / len(times)

def request_to_string(req):
    return '{}\n{}\n{}\n\n{}'.format(
        req.method + ' / HTTP/1.1',
        'Host: ' + req.url,
        '\n'.join('{}: {}'.format(k, v) for k, v in req.headers.items()),
        req.body,
    )

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print "Usage: ./links_benchmark.py [links file] [optional: results output name]"
        sys.exit(0)

    try:
        linksFile = open(sys.argv[1], "r")
    except:
        print "Error opening: ", sys.argv[1]
        sys.exit(0)

    if len(sys.argv) > 2:
        outExt = sys.argv[2]
    else:
        outExt = None

    writer = gnsq.Nsqd(address=NSQ_ADDR, tcp_port=NSQ_PORT)

    links = set()
    for line in linksFile:
        link = line.strip().strip('"')
        if len(link) > 0 and link != "null":
            links.add(link)

    benchmark = LinksBenchmark(outExt)

    reader = gnsq.Reader('links_out_', 'links_test', '%s:%d' % (NSQ_ADDR, NSQ_PORT))

    @reader.on_message.connect
    def handler(reader, message):
        response = message.body

        #print response

        keyLoc = response.find('X-Bn-Event-Id:')
        resultLoc = response.find('{')

        if keyLoc > 0 and resultLoc > 0:
            keyLoc = keyLoc + len('X-Bn-Event-Id: ')
            key = response[keyLoc:keyLoc+32]
            #print 'Key: ', key
            result = json.loads( response[resultLoc:] )
            #print result
            benchmark.add_result(key, result)
        else:
            print "Unknown result! ", response

        message.finish()

        if benchmark.successes >= benchmark.attempts:
            print "Done!"
            benchmark.print_stats()

            reader.close()

    # send requests
    for link in links:
        request = json.dumps( { "request" : [{ 'url' : link}] } )
        
        httpReq = requests.Request('POST', LINKS_ADDR, headers={
            'Content-Type' : 'application/json',
            'X-Bn-Event-Id' : hashlib.md5(link).hexdigest(),
            'X-Bn-Timeout' : '5s',
            'User-Agent' : 'python http',
            'Accept-Encoding' : 'gzip',
        }, data=request).prepare()

        reqStr = request_to_string(httpReq)
        result = writer.publish('links', reqStr)
        if result == 'OK':
            benchmark.inc_attempts()

    benchmark.print_stats()
    print 'All requests sent... getting responses...'    

    def on_kill(signal, frame):
        print "Terminating..."
        benchmark.print_stats()
        sys.exit(0)

    signal.signal(signal.SIGINT, on_kill)

    reader.start()

