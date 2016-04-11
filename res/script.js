function setSrcQuery(e, q) {
	var src  = e.src;
	var p = src.indexOf('?');
	if (p >= 0) {
		src = src.substr(0, p);
	}
	e.src = src + "?" + q
}
function playAudio() {
	var le = document.getElementById("lang");
	var lang = le.options[le.selectedIndex].value;
	var e = document.getElementById('audio')
	setSrcQuery(e, "lang=" + lang)
	e.style.display = 'block';
	e.autoplay = 'true';
	return false;
}
function changeLang() {
	var e = document.getElementById('audio')
	if (e.style.display == 'block') {
		playAudio();
	}
}
function reload() {
	setSrcQuery(document.getElementById('image'), "reload=" + (new Date()).getTime());
	setSrcQuery(document.getElementById('audio'), (new Date()).getTime());
	return false;
}