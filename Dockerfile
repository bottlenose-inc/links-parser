FROM docker.bottlenose.com/image/alpine-base

RUN set -x;\
  echo http://nl.alpinelinux.org/alpine/edge/community >> /etc/apk/repositories

ADD ./ /links-parser
WORKDIR /links-parser

RUN apk -U add make gcc g++ icu-dev ncurses-dev git bash go hydrant curl \
  && cd $PROJECT_PATH && \
  cd /links-parser && GOPATH=/go make && apk del make icu-dev ncurses-dev git go curl

CMD /links-parser/links-parser >> /links-parser/log/links-parser.log

