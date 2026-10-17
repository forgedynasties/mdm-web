// Loaded by templates/device.html.
/* Same dark palette as the Overview fleet map (templates/overview.html
     OV_MAP_DARK) — kept in sync there; duplicated here since each page loads
     the Maps JS API independently. */
var DEV_MAP_DARK = [
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
function devMapStyles(){ return (window.mdmTheme && window.mdmTheme()==='dark') ? DEV_MAP_DARK : []; }
function devInitMap(){
  var el = document.getElementById('dev-map');
  if (!el || !window.google || !google.maps) return;
  var pos = {lat: parseFloat(window.DEVICE_MAP.lat), lng: parseFloat(window.DEVICE_MAP.lng)};
  var map = new google.maps.Map(el, {
    zoom: 16, center: pos, mapTypeControl:false, streetViewControl:false,
    fullscreenControl:false, keyboardShortcuts:false, styles: devMapStyles()
  });
  window.addEventListener('mdm:themechange', function(){ map.setOptions({styles: devMapStyles()}); });
  new google.maps.Marker({ position: pos, map: map, title: window.DEVICE_MAP.serial });
}
