package cmd

import (
	"context"
	"os"
	"path/filepath"
)

func (ap *app) writeHtml(ctx context.Context) error {
	fName := filepath.Join(ap.cfg.dataDir, "index.html")
	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer f.Close()
	if !ap.cfg.avoidTimeProgress {
		ap.narrate(I, "📄", "index: %s", fName)
	}
	f.WriteString(reportHTML)
	return nil
}

const reportHTML = `<!DOCTYPE html>
<html lang="en">
<head>
 <meta charset="utf-8"/>
 <title>shakespeare report</title>
 <link href="https://fonts.googleapis.com/css?family=Nova+Mono&display=swap" rel="stylesheet">
 <link href="https://fonts.googleapis.com/css?family=Pinyon+Script&display=swap" rel="stylesheet">
 <script src="https://cdnjs.cloudflare.com/ajax/libs/jquery/3.1.1/jquery.min.js"></script>
 <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/jstree/3.3.8/themes/default/style.min.css" />
 <script src="https://cdnjs.cloudflare.com/ajax/libs/jstree/3.3.8/jstree.min.js"></script>
 <script src="https://kit.fontawesome.com/da97b0d013.js"></script>
 <script src="result.js" type="text/javascript"></script>
 <style type='text/css'>
h1,h3,p,ul#refs{text-align: center;}
p,ul#refs{font-family:'Pinyon Script',cursive;font-size: large;}
#artifacts,pre,code{font-family: 'Nova Mono',monospace;font-size:small;}
pre{overflow:auto;}
.divider{font-family:serif; margin-top:3em; margin-bottom:3em;}
.divider:before{content:"⊱ ────────────────── {.⋅ ♫ ⋅.} ───────────────── ⊰";}
.result{font-weight:bold;}
.good{color:green;}
.bad{color:blue;}
.kw{font-weight:bold;}
.rn{color:blue;font-style:italic;}
.acn{color:blue;font-style:italic;font-weight:bold;}
.sn{color:darkgreen;font-style:italic;}
.an{color:purple;font-style:italic;}
.ann{color:orange;font-style:italic;}
.sh{color:#444;}
.re{color:green;}
.mod{font-style:italic;}
.centersvg{margin-left: auto; margin-right: auto; max-width: 800px;}
  </style>
</head>
<body>
 <h1 id=ttitle>A tale of <span id=title>surprise</span></h1>
 <h3 id=tauthors>Written by <span id=authors>the collective</span></h3>

 <p>The day was a <span id=date>beautiful day;</span>
 on that fateful day, the story began...</p>

 <p class="result good" id=resultgood>🎉 Rejoice! This tale ends well.</p>
 <p class="result bad" id=resultbad>😭 Avert your eyes! For this tale, alas, does not end well.</p>

 <p class=divider></p>
 <p>This performance lasted <span id=duration>forever</span>.</p>
 <div class=centersvg id=mainsvg></div>

 <div id=mayberepeat>
  <p class=divider></p>
  <p>For your delicate eyes, the last <span id=rduration>moments</span> of the play:</p>
  <div class=centersvg id=repeatsvg></div>
 </div>

 <div id=maybeerror>
  <p class=divider></p>
  <p>A tragic ending!</p>
  <div class=centersvg><pre id=error></pre></div>
 </div>

 <div id=mayberef>
  <p class=divider></p>
  <p>Attention! You may want to know:</p>
  <ul id=refs></ul>
 </div>

 <p class=divider></p>
 <p>For your curious eyes, the full book for this play:</p>
 <div class=centersvg><pre id=config></pre></div>

 <p class=divider></p>
 <p>For your inquisitive eyes, the artifacts for this play:</p>
 <div class=centersvg><div id=artifacts></div></div>

 <p class=divider></p>
 <p><em><small>A report produced by <a href='https://github.com/knz/shakespeare'>Shakespeare</a>,
 <span id=version>unknown version</span></small></em></p>

<script type="text/javascript">
$(function() {
  var icon = "🎉";
  var adj = "merry";
  if (result.Foul) {
     adj = "tragic";
     icon = "😭";
  }
  document.title = icon + " shakespeare report";
  if (result.Title) {
    $("#title").text(result.Title);
    document.title = document.title + ": a " + adj + " tale of " + result.Title;
  } else {
    $("#ttitle").hide();
  }
  if (result.Authors) {
    $("#authors").text(result.Authors);
    $("<meta/>", {name:"author", content:result.Authors}).appendTo("head");
  } else {
    $("#tauthors").hide();
  }
  if (result.Foul) {
    $("#resultgood").hide()
  } else {
    $("#resultbad").hide()
  }
  var extraDur = "";
  if (result.Repeat != null && result.Repeat.NumRepeats > 0) {
     var r = result.Repeat;
     extraDur = ", including " + r.NumRepeats;
     extraDur += " iterations of acts " + r.FirstRepeatedAct;
     extraDur += "-" + r.LastRepeatedAct;
  }
	for (var i in result.Artifacts) {
		var artifact = result.Artifacts[i];
    if (artifact.text == "plot.svg") {
      $("<embed/>", { src: artifact.text }).appendTo("#mainsvg");
    }
  }
  if (result.Repeat != null) {
    var r = result.Repeat;
    $("#rduration").text(r.Duration.toFixed(2) + "s");
  	for (var i in r.Artifacts) {
	  	var artifact = r.Artifacts[i];
      if (artifact.text == "lastplot.svg") {
        $("<embed/>", { src: artifact.text }).appendTo("#repeatsvg");
      }
    }
  } else {
    $("#mayberepeat").hide();
  }
  if (result.Error) {
    $("#error").text(result.Error);
  } else {
    $("#maybeerror").hide();
  }
  if (result.SeeAlso) {
    for (var i in result.SeeAlso) {
       var ref = result.SeeAlso[i];
       var i = $("<li/>");
       if (ref.indexOf("://") >= 0) {
         var a = $("<a/>", {href:ref});
         $("<code>").text(ref).appendTo(a);
         a.appendTo(i);
      } else {
         i.text(ref);
      }
      i.appendTo("#refs");
    }
  } else {
    $("#mayberef").hide();
  }
  $("#duration").text(result.PlayDurationVerbose + extraDur);
  $("#date").html(result.TimestampHTML);
  $("#version").text(result.Version);
  $("#config").html(result.ConfigHTML + result.StepsHTML);
  var items = result.Artifacts;
  $("#artifacts").jstree({
    core: {
      data: items,
      multiple: false,
      plugins: ["themes","sort"]
    }
   });
 $("#artifacts").on('select_node.jstree', function(e, data) {
     if (!data.node.original.IsDir) {
       data.node.a_attr.href = data.node.original.Path;
       window.open(data.node.a_attr.href, "_new");
     }
  });

 });
</script>
</body>
</html>
`
