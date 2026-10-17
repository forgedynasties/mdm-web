// Loaded by templates/overview.html.
      // Google-recommended dark map palette, applied when the dashboard is in dark mode
      // and re-applied live on theme toggle (mdm:themechange).
      var OV_MAP_DARK = [
        {elementType:'geometry',stylers:[{color:'#212a37'}]},
        {elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},
        {elementType:'labels.text.stroke',stylers:[{color:'#1b2431'}]},
        {featureType:'administrative',elementType:'geometry',stylers:[{color:'#4b5563'}]},
        {featureType:'poi',stylers:[{visibility:'off'}]},
        {featureType:'road',elementType:'geometry',stylers:[{color:'#2c3644'}]},
        {featureType:'road',elementType:'labels.text.fill',stylers:[{color:'#9ca5b3'}]},
        {featureType:'road.highway',elementType:'geometry',stylers:[{color:'#3a4757'}]},
        {featureType:'transit',stylers:[{visibility:'off'}]},
        {featureType:'water',elementType:'geometry',stylers:[{color:'#17212f'}]},
        {featureType:'water',elementType:'labels.text.fill',stylers:[{color:'#51607a'}]}
      ];
      function ovMapStyles(){ return (window.mdmTheme && window.mdmTheme()==='dark') ? OV_MAP_DARK : []; }
      function ovInitMap(){
        var el = document.getElementById('ov-map');
        if (!el || !window.google || !google.maps) return;
        function esc(s){ return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];}); }
        var pts = [];
        try { pts = JSON.parse(document.getElementById('ov-map-data').textContent || '[]'); } catch(e){}
        var map = new google.maps.Map(el, {
          zoom: 2, center: {lat:20, lng:0},
          mapTypeControl:false, streetViewControl:false, fullscreenControl:false, keyboardShortcuts:false,
          styles: ovMapStyles()
        });
        window.addEventListener('mdm:themechange', function(){ map.setOptions({styles: ovMapStyles()}); });
        var bounds = new google.maps.LatLngBounds();
        var iw = new google.maps.InfoWindow();
        var markers = [];
        pts.forEach(function(p){
          var pos = {lat:p.lat, lng:p.lon};
          var mk = new google.maps.Marker({
            position: pos, map: map, title: p.name,
            icon: {path: google.maps.SymbolPath.CIRCLE, scale:7,
              fillColor: p.online ? '#22c55e' : '#94a3b8', fillOpacity:1,
              strokeColor:'#fff', strokeWeight:2}
          });
          mk.addListener('click', function(){
            iw.setContent('<div style="min-width:150px"><b>'+esc(p.name)+'</b>'
              + '<div style="opacity:.7">'+esc(p.serial)+'</div>'
              + (p.address ? '<div>'+esc(p.address)+'</div>' : '')
              + '<div style="margin:3px 0">'+(p.online?'🟢 online':'⚪ offline')+'</div>'
              + '<a href="/devices/'+encodeURIComponent(p.serial)+'">Open device →</a></div>');
            iw.open(map, mk);
          });
          bounds.extend(pos);
          markers.push(mk);
        });
        // Group markers that are close together into a numbered cluster at low zoom;
        // clicking a cluster (its default behavior) zooms in until it splits apart —
        // otherwise devices at ~the same address just stack invisibly on top of each other.
        // Custom renderer: the library's default cluster icon is a plain generic
        // circle+pin that doesn't match the app's look — draw our own brand-colored
        // dot with a crisp white count label instead.
        if (window.markerClusterer && markers.length > 1) {
          var accent = getComputedStyle(document.body).getPropertyValue('--accent').trim() || '#f9674e';
          new markerClusterer.MarkerClusterer({
            map: map, markers: markers,
            renderer: {
              render: function (cluster) {
                var count = cluster.count;
                var size = count < 10 ? 34 : count < 100 ? 42 : 50;
                var r = size / 2;
                var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="' + size + '" height="' + size + '">' +
                  '<circle cx="' + r + '" cy="' + r + '" r="' + (r - 2) + '" fill="' + accent + '" stroke="#fff" stroke-width="2.5"/>' +
                  '</svg>';
                return new google.maps.Marker({
                  position: cluster.position,
                  icon: { url: 'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(svg), scaledSize: new google.maps.Size(size, size), anchor: new google.maps.Point(r, r) },
                  label: { text: String(count), color: '#fff', fontSize: '12.5px', fontWeight: '700' },
                  zIndex: 1000 + count
                });
              }
            }
          });
        }
        if (pts.length === 1) { map.setCenter({lat:pts[0].lat, lng:pts[0].lon}); map.setZoom(12); }
        else if (pts.length > 1) {
          // fitBounds on a near-zero-area box (devices clustered at ~the same spot)
          // zooms to Google's max (~21, building-level) — clamp it back to a
          // neighborhood-level view so the surrounding area is still visible.
          google.maps.event.addListenerOnce(map, 'bounds_changed', function(){
            if (map.getZoom() > 14) map.setZoom(14);
          });
          map.fitBounds(bounds);
        }
      }
      
