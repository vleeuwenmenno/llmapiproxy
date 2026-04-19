const CACHE_NAME = 'ai-chat-v1';
const STATIC_ASSETS = [
  '/ui/chat',
  '/ui/static/main.css',
  '/ui/static/chat.css',
  '/ui/static/models.css',
  '/ui/static/settings.css',
  '/ui/static/dashboard.css'
];

// Install: cache static assets
self.addEventListener('install', function(event) {
  event.waitUntil(
    caches.open(CACHE_NAME).then(function(cache) {
      return cache.addAll(STATIC_ASSETS);
    })
  );
  self.skipWaiting();
});

// Activate: clean old caches
self.addEventListener('activate', function(event) {
  event.waitUntil(
    caches.keys().then(function(cacheNames) {
      return Promise.all(
        cacheNames.filter(function(name) {
          return name !== CACHE_NAME;
        }).map(function(name) {
          return caches.delete(name);
        })
      );
    })
  );
  self.clients.claim();
});

// Fetch: cache-first for static assets, network-first for API
self.addEventListener('fetch', function(event) {
  var url = new URL(event.request.url);

  // Skip non-GET requests
  if (event.request.method !== 'GET') return;

  // API calls: network only
  if (url.pathname.startsWith('/v1/')) {
    event.respondWith(fetch(event.request));
    return;
  }

  // Static assets: cache-first
  if (url.pathname.startsWith('/ui/static/')) {
    event.respondWith(
      caches.match(event.request).then(function(response) {
        return response || fetch(event.request).then(function(fetchResponse) {
          return caches.open(CACHE_NAME).then(function(cache) {
            cache.put(event.request, fetchResponse.clone());
            return fetchResponse;
          });
        });
      })
    );
    return;
  }

  // HTML pages: network-first with cache fallback
  event.respondWith(
    fetch(event.request).then(function(response) {
      var responseClone = response.clone();
      caches.open(CACHE_NAME).then(function(cache) {
        cache.put(event.request, responseClone);
      });
      return response;
    }).catch(function() {
      return caches.match(event.request).then(function(response) {
        return response || caches.match('/ui/chat');
      });
    })
  );
});
