# Stage 1: Build the plugin
FROM registry.access.redhat.com/ubi9/nodejs-18:latest AS build

USER 0
WORKDIR /opt/app-root/src

COPY package.json yarn.lock ./
RUN npm install -g yarn && yarn install --frozen-lockfile --network-timeout 600000

COPY . .
RUN yarn build

# Stage 2: Serve with nginx
FROM registry.access.redhat.com/ubi9/nginx-120:latest

USER 0

COPY --from=build /opt/app-root/src/dist /opt/app-root/src
COPY nginx.conf /etc/nginx/nginx.conf

# service-ca operator mounts TLS certs here
RUN mkdir -p /var/cert && chown 1001:0 /var/cert

USER 1001

CMD ["nginx", "-g", "daemon off;"]
